// Command newgit is a small GitLab-style Git host.
//
//	newgit serve -addr :8080 -data ./data -base-url http://localhost:8080
//	newgit hook <name>   (run by git through core.hooksPath)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AlphaTechini/newgit/internal/auth"
	"github.com/AlphaTechini/newgit/internal/githttp"
	"github.com/AlphaTechini/newgit/internal/gitx"
	"github.com/AlphaTechini/newgit/internal/hooks"
	"github.com/AlphaTechini/newgit/internal/mcpserver"
	"github.com/AlphaTechini/newgit/internal/seed"
	"github.com/AlphaTechini/newgit/internal/store"
	"github.com/AlphaTechini/newgit/internal/web"
)

const version = "0.1.0"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		if len(os.Args) < 3 {
			os.Exit(1)
		}
		os.Exit(hooks.Run(os.Args[2]))
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("NEWGIT_ADDR", ":8080"), "listen address")
	dataDir := fs.String("data", envOr("NEWGIT_DATA", "./data"), "data directory")
	baseURL := fs.String("base-url", envOr("NEWGIT_BASE_URL", ""), "public base URL, e.g. https://git.example.com (default http://localhost<addr>)")
	maxPush := fs.Int64("max-push-mb", 200, "largest accepted push pack in MiB")
	seedDemo := fs.Bool("seed-demo", false, "create a demo user, repository, issues and merge requests if the data dir has none")
	_ = fs.Parse(args)

	if *baseURL == "" {
		host := *addr
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		*baseURL = "http://" + host
	}
	*baseURL = strings.TrimRight(*baseURL, "/")
	if err := run(*addr, *dataDir, *baseURL, *maxPush, *seedDemo); err != nil {
		log.Fatal(err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func checkGit() error {
	out, err := exec.Command("git", "version").Output()
	if err != nil {
		return fmt.Errorf("git is required on PATH: %w", err)
	}
	// "git version 2.43.0"
	f := strings.Fields(string(out))
	if len(f) < 3 {
		return nil
	}
	parts := strings.Split(f[2], ".")
	if len(parts) >= 2 {
		maj, _ := strconv.Atoi(parts[0])
		min, _ := strconv.Atoi(parts[1])
		if maj < 2 || (maj == 2 && min < 38) {
			return fmt.Errorf("git %s is too old: NewGit needs git 2.38+ (for merge-tree --write-tree)", f[2])
		}
	}
	log.Printf("using %s", strings.TrimSpace(string(out)))
	return nil
}

func run(addr, dataDir, baseURL string, maxPushMB int64, seedDemo bool) error {
	if err := checkGit(); err != nil {
		return err
	}
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	runDir := filepath.Join(dataDir, "run")
	hooksDir := filepath.Join(dataDir, "hooks")
	for _, d := range []string{dataDir, runDir, hooksDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	// App-owned git config used by every git child (GIT_CONFIG_GLOBAL with
	// GIT_CONFIG_NOSYSTEM=1), so no user or system config can interfere.
	gitConfig := filepath.Join(dataDir, "gitconfig")
	cfg := fmt.Sprintf(`[core]
	hooksPath = %s
	logAllRefUpdates = false
[receive]
	fsckObjects = true
	maxInputSize = %d
	procReceiveRefs = refs/for
	advertisePushOptions = true
	denyNonFastForwards = false
	denyDeletes = false
[uploadpack]
	allowFilter = true
	allowAnySHA1InWant = true
[gc]
	auto = 0
[user]
	name = NewGit
	email = system@newgit
[safe]
	directory = *
`, quote(hooksDir), maxPushMB<<20)
	if err := os.WriteFile(gitConfig, []byte(cfg), 0o640); err != nil {
		return err
	}
	gitx.Configure(gitConfig)
	for _, h := range []string{"pre-receive", "proc-receive"} {
		script := fmt.Sprintf("#!/bin/sh\nexec %s hook %s\n", shellQuote(exe), h)
		if err := os.WriteFile(filepath.Join(hooksDir, h), []byte(script), 0o750); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, dataDir)
	if err != nil {
		return err
	}
	if seedDemo {
		if err := seed.Demo(ctx, st, baseURL); err != nil {
			return err
		}
	}
	secure := strings.HasPrefix(baseURL, "https://")
	a, err := auth.New(filepath.Join(dataDir, "session.key"), secure)
	if err != nil {
		return err
	}

	// Internal Unix socket for hooks (AGit proc-receive callbacks).
	sock := filepath.Join(runDir, "hooks.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	_ = os.Chmod(sock, 0o600)
	internal := http.NewServeMux()
	internal.HandleFunc("/internal/agit", func(w http.ResponseWriter, r *http.Request) {
		var req hooks.AGitRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		u, ok := st.UserByID(req.UserID)
		if !ok {
			http.Error(w, "unknown user", 403)
			return
		}
		res := st.HandleAGit(r.Context(), req.RepoID, u, req.Commands, req.Options, baseURL)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
	go func() { _ = http.Serve(ln, internal) }()

	gs := &githttp.Server{Store: st, HookSocket: sock}
	mcpH := mcpserver.Handler(st, baseURL, version)
	ws, err := web.New(st, a, gs, baseURL, mcpH)
	if err != nil {
		return err
	}

	// Maintenance: commit-graph with Bloom filters soon after pushes, full
	// repack with bitmaps every few hours.
	go func() {
		quick := time.NewTicker(30 * time.Second)
		full := time.NewTicker(3 * time.Hour)
		defer quick.Stop()
		defer full.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-quick.C:
				st.RunMaintenance(context.Background(), false)
			case <-full.C:
				for _, r := range st.ListRepos("") {
					st.MarkDirty(r.ID)
				}
				st.RunMaintenance(context.Background(), true)
			}
		}
	}()

	srv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(ws.Handler()),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		ln.Close()
	}()
	log.Printf("NewGit %s listening on %s (public URL %s, data %s)", version, addr, baseURL, dataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/-/static/") {
			return
		}
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond))
	})
}
