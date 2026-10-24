// Package githttp serves Git's smart HTTP protocol by running
// `git upload-pack` / `git receive-pack --stateless-rpc` directly with
// os/exec. It deliberately does not use net/http/cgi with git http-backend:
// that handler rejects chunked request bodies, which git sends for any push
// larger than http.postBuffer (1 MiB).
package githttp

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/AlphaTechini/newgit/internal/auth"
	"github.com/AlphaTechini/newgit/internal/gitx"
	"github.com/AlphaTechini/newgit/internal/store"
)

// Server serves git over HTTP.
type Server struct {
	Store      *store.Store
	HookSocket string
}

// IsGitRequest reports whether the path suffix is a smart HTTP endpoint.
func IsGitRequest(rest string) bool {
	return rest == "info/refs" || rest == "git-upload-pack" || rest == "git-receive-pack"
}

func challenge(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="NewGit (use a personal access token as the password)"`)
	http.Error(w, msg, http.StatusUnauthorized)
}

// authPush authenticates a push. Returns the user or writes a 401/403.
func (s *Server) authPush(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	tok, ok := auth.BasicToken(r)
	if !ok {
		challenge(w, "Authentication required to push. Create a token on your device page and use it as the password.")
		return store.User{}, false
	}
	u, t, ok := s.Store.VerifyToken(tok)
	if !ok {
		challenge(w, "Invalid or revoked token.")
		return store.User{}, false
	}
	if !t.CanWrite() {
		http.Error(w, "This token is read-only.", http.StatusForbidden)
		return store.User{}, false
	}
	return u, true
}

// Serve handles one smart HTTP request for repo. rest is the path after
// "<owner>/<repo>.git/".
func (s *Server) Serve(w http.ResponseWriter, r *http.Request, repo store.Repo, rest string) {
	switch {
	case rest == "info/refs" && r.Method == http.MethodGet:
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			http.Error(w, "dumb HTTP protocol is not supported; use a git client 1.6.6 or newer", http.StatusForbidden)
			return
		}
		if service == "git-receive-pack" {
			if _, ok := s.authPush(w, r); !ok {
				return
			}
		}
		s.infoRefs(w, r, repo, service)
	case rest == "git-upload-pack" && r.Method == http.MethodPost:
		s.rpc(w, r, repo, "upload-pack", nil, store.User{})
	case rest == "git-receive-pack" && r.Method == http.MethodPost:
		u, ok := s.authPush(w, r)
		if !ok {
			return
		}
		unlock, err := s.Store.PushLock(repo.ID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer unlock()
		before := store.SnapshotRefs(r.Context(), repo.Dir)
		canWrite := "0"
		if repo.CanWrite(u.ID) {
			canWrite = "1"
		}
		env := []string{
			"NEWGIT_USER_ID=" + u.ID,
			"NEWGIT_USERNAME=" + u.Username,
			"NEWGIT_REPO_ID=" + repo.ID,
			"NEWGIT_CAN_WRITE=" + canWrite,
			"NEWGIT_SOCKET=" + s.HookSocket,
		}
		if ok := s.rpc(w, r, repo, "receive-pack", env, u); ok {
			// Record who pushed what, and refresh branch-based MRs, using a
			// fresh context: the client may already have hung up.
			after := store.SnapshotRefs(context.Background(), repo.Dir)
			s.Store.RecordPush(context.Background(), repo.ID, u, "git", before, after)
		}
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

func protocolEnv(r *http.Request) []string {
	gp := r.Header.Get("Git-Protocol")
	if gp == "" {
		return nil
	}
	// Only pass well-formed values like "version=2".
	for _, part := range strings.Split(gp, ":") {
		for _, c := range part {
			if !(c == '=' || c == '.' || c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				return nil
			}
		}
	}
	return []string{"GIT_PROTOCOL=" + gp}
}

func (s *Server) infoRefs(w http.ResponseWriter, r *http.Request, repo store.Repo, service string) {
	sub := strings.TrimPrefix(service, "git-")
	cmd := gitx.Cmd(r.Context(), repo.Dir, sub, "--stateless-rpc", "--advertise-refs", ".")
	cmd.Env = append(cmd.Env, protocolEnv(r)...)
	out, err := cmd.Output()
	if err != nil {
		log.Printf("info/refs %s %s: %v", repo.Path(), service, err)
		http.Error(w, "git error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	isV2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2") && service == "git-upload-pack"
	if !isV2 {
		io.WriteString(w, pktLine("# service="+service+"\n"))
		io.WriteString(w, "0000")
	}
	w.Write(out)
}

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// rpc runs upload-pack or receive-pack in stateless-rpc mode.
func (s *Server) rpc(w http.ResponseWriter, r *http.Request, repo store.Repo, sub string, env []string, u store.User) bool {
	ctype := "application/x-git-" + sub + "-request"
	if r.Header.Get("Content-Type") != ctype {
		http.Error(w, "bad content type", http.StatusUnsupportedMediaType)
		return false
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return false
		}
		defer gz.Close()
		body = gz
	}
	// Do not tie the git process to the request context for receive-pack: a
	// client disconnect mid-push must not leave refs half-updated.
	ctx := r.Context()
	if sub == "receive-pack" {
		ctx = context.Background()
	}
	cmd := gitx.Cmd(ctx, repo.Dir, sub, "--stateless-rpc", ".")
	cmd.Env = append(cmd.Env, protocolEnv(r)...)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = body
	w.Header().Set("Content-Type", "application/x-git-"+sub+"-result")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	f, _ := w.(http.Flusher)
	cmd.Stdout = flushWriter{w: w, f: f}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Printf("%s %s (user %q): %v: %s", sub, repo.Path(), u.Username, err, strings.TrimSpace(stderr.String()))
		// Output may already be partially written; the client sees the error
		// through the protocol's own status reporting.
		return sub == "receive-pack"
	}
	return true
}
