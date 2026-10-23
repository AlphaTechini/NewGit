// Package hooks implements `newgit hook <name>`, which git's receive-pack
// runs through core.hooksPath.
//
//   - pre-receive enforces ref policy (fails closed).
//   - proc-receive implements the AGit flow: pushes to refs/for/<branch> are
//     handed to the server, which opens or updates a merge request.
package hooks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AlphaTechini/newgit/internal/store"
)

// Run executes the named hook and returns the process exit code.
func Run(name string) int {
	switch name {
	case "pre-receive":
		return preReceive(os.Stdin, os.Stderr)
	case "proc-receive":
		return procReceive(os.Stdin, os.Stdout, os.Stderr)
	default:
		return 0
	}
}

func preReceive(in io.Reader, errw io.Writer) int {
	repoID := os.Getenv("NEWGIT_REPO_ID")
	if repoID == "" || os.Getenv("NEWGIT_USER_ID") == "" {
		fmt.Fprintln(errw, "NewGit: push rejected: not running under the NewGit server")
		return 1
	}
	canWrite := os.Getenv("NEWGIT_CAN_WRITE") == "1"
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	ok := true
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			fmt.Fprintln(errw, "NewGit: malformed pre-receive input")
			return 1
		}
		ref := f[2]
		switch {
		case strings.HasPrefix(ref, "refs/newgit/"), strings.HasPrefix(ref, "refs/merge-requests/"):
			fmt.Fprintf(errw, "NewGit: %s is managed by the server and cannot be pushed\n", ref)
			ok = false
		case strings.HasPrefix(ref, "refs/for/"):
			// handled by proc-receive
		case !canWrite:
			fmt.Fprintf(errw, "NewGit: only the repository owner can push to %s.\nNewGit: to propose this change, push it as a merge request:\nNewGit:   git push origin HEAD:refs/for/main -o title=\"...\"\n", ref)
			ok = false
		case strings.HasPrefix(ref, "refs/heads/"), strings.HasPrefix(ref, "refs/tags/"):
		default:
			fmt.Fprintf(errw, "NewGit: pushing to %s is not allowed (only branches and tags)\n", ref)
			ok = false
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(errw, "NewGit: could not read pre-receive input:", err)
		return 1
	}
	if !ok {
		return 1
	}
	return 0
}

// ---------- pkt-line ----------

type pkt struct {
	flush bool
	data  string
}

func readPkt(r *bufio.Reader) (pkt, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return pkt{}, err
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil {
		return pkt{}, fmt.Errorf("bad pkt-line header %q", hdr)
	}
	if n == 0 {
		return pkt{flush: true}, nil
	}
	if n < 4 {
		return pkt{}, fmt.Errorf("bad pkt-line length %d", n)
	}
	buf := make([]byte, n-4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return pkt{}, err
	}
	return pkt{data: strings.TrimSuffix(string(buf), "\n")}, nil
}

func writePkt(w io.Writer, s string) {
	fmt.Fprintf(w, "%04x%s", len(s)+5, s+"\n")
}

func flushPkt(w io.Writer) { io.WriteString(w, "0000") }

// readUntilFlush reads data packets until a flush packet.
func readUntilFlush(r *bufio.Reader) ([]string, error) {
	var out []string
	for {
		p, err := readPkt(r)
		if err != nil {
			return nil, err
		}
		if p.flush {
			return out, nil
		}
		out = append(out, p.data)
	}
}

func procReceive(in io.Reader, out io.Writer, errw io.Writer) int {
	r := bufio.NewReader(in)
	// 1. version negotiation
	lines, err := readUntilFlush(r)
	if err != nil || len(lines) == 0 || !strings.HasPrefix(lines[0], "version=1") {
		fmt.Fprintln(errw, "NewGit: proc-receive: bad version negotiation")
		return 1
	}
	gitHasOptions := false
	if i := strings.IndexByte(lines[0], 0); i >= 0 {
		for _, f := range strings.Fields(lines[0][i+1:]) {
			if f == "push-options" {
				gitHasOptions = true
			}
		}
	}
	if gitHasOptions {
		writePkt(out, "version=1\x00push-options")
	} else {
		writePkt(out, "version=1")
	}
	flushPkt(out)
	// 2. commands
	cmdLines, err := readUntilFlush(r)
	if err != nil {
		fmt.Fprintln(errw, "NewGit: proc-receive: reading commands:", err)
		return 1
	}
	var cmds []store.AGitCommand
	for _, l := range cmdLines {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		cmds = append(cmds, store.AGitCommand{Old: f[0], New: f[1], Ref: f[2]})
	}
	// 3. push options
	opts := map[string]string{}
	if gitHasOptions {
		optLines, err := readUntilFlush(r)
		if err != nil {
			fmt.Fprintln(errw, "NewGit: proc-receive: reading push options:", err)
			return 1
		}
		for _, o := range optLines {
			k, v, _ := strings.Cut(o, "=")
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if old, ok := opts[k]; ok && k == "description" {
				v = old + "\n" + v
			}
			opts[k] = v
		}
	}
	// 4. ask the server
	results, err := callServer(cmds, opts)
	if err != nil {
		for _, c := range cmds {
			writePkt(out, "ng "+c.Ref+" newgit server unavailable")
		}
		flushPkt(out)
		fmt.Fprintln(errw, "NewGit: proc-receive:", err)
		return 0
	}
	for _, res := range results {
		if !res.OK {
			writePkt(out, "ng "+res.Ref+" "+oneLine(res.Reason))
			continue
		}
		writePkt(out, "ok "+res.Ref)
		if res.RefName != "" {
			writePkt(out, "option refname "+res.RefName)
		}
		if res.OldOID != "" {
			writePkt(out, "option old-oid "+res.OldOID)
		}
		if res.NewOID != "" {
			writePkt(out, "option new-oid "+res.NewOID)
		}
		if res.Forced {
			writePkt(out, "option forced-update")
		}
		if res.Message != "" {
			for _, l := range strings.Split(res.Message, "\n") {
				fmt.Fprintln(errw, l)
			}
		}
	}
	flushPkt(out)
	return 0
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return "rejected"
	}
	return s
}

// AGitRequest is sent from the hook to the server over the Unix socket.
type AGitRequest struct {
	RepoID   string              `json:"repo_id"`
	UserID   string              `json:"user_id"`
	Commands []store.AGitCommand `json:"commands"`
	Options  map[string]string   `json:"options"`
}

func callServer(cmds []store.AGitCommand, opts map[string]string) ([]store.AGitResult, error) {
	sock := os.Getenv("NEWGIT_SOCKET")
	if sock == "" {
		return nil, fmt.Errorf("NEWGIT_SOCKET not set")
	}
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}
	body, _ := json.Marshal(AGitRequest{RepoID: os.Getenv("NEWGIT_REPO_ID"), UserID: os.Getenv("NEWGIT_USER_ID"), Commands: cmds, Options: opts})
	resp, err := client.Post("http://newgit/internal/agit", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("server returned %s: %s", resp.Status, b)
	}
	var results []store.AGitResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}
	return results, nil
}
