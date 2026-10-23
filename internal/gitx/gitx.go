// Package gitx runs the git binary with a locked-down environment and parses
// its output. All repository access in NewGit goes through here so that git's
// own performance features (commit-graph, Bloom filters, bitmaps) apply.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ZeroOID is the all-zero object id used by git for "no object".
const ZeroOID = "0000000000000000000000000000000000000000"

// Env holds the process-wide git settings.
type Env struct {
	GlobalConfig string // path to the app-owned git config
}

var defaultEnv Env

// Configure sets the global config path used for every git child.
func Configure(globalConfig string) { defaultEnv.GlobalConfig = globalConfig }

// BaseEnv returns the environment every git child runs with.
func BaseEnv(gitDir string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		// Never inherit git variables from our own environment.
		if strings.HasPrefix(k, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	if defaultEnv.GlobalConfig != "" {
		env = append(env, "GIT_CONFIG_GLOBAL="+defaultEnv.GlobalConfig)
	}
	if gitDir != "" {
		env = append(env, "GIT_DIR="+gitDir)
	}
	return env
}

// Cmd builds a git command for a bare repository.
func Cmd(ctx context.Context, gitDir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = BaseEnv(gitDir)
	if gitDir != "" {
		cmd.Dir = gitDir
	}
	return cmd
}

// Error is returned when git exits non-zero.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

// RunOpts customises Run.
type RunOpts struct {
	Stdin io.Reader
	Env   []string // extra env
}

// Run executes git and returns stdout.
func Run(ctx context.Context, gitDir string, opts *RunOpts, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := Cmd(ctx, gitDir, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if opts != nil {
		cmd.Stdin = opts.Stdin
		cmd.Env = append(cmd.Env, opts.Env...)
	}
	if err := cmd.Run(); err != nil {
		return out.Bytes(), &Error{Args: args, Stderr: errb.String(), Err: err}
	}
	return out.Bytes(), nil
}

// RunString runs git and returns trimmed stdout.
func RunString(ctx context.Context, gitDir string, args ...string) (string, error) {
	b, err := Run(ctx, gitDir, nil, args...)
	return strings.TrimSpace(string(b)), err
}

// ExitCode returns the exit code of a git error, or -1.
func ExitCode(err error) int {
	var ge *Error
	if errors.As(err, &ge) {
		var ee *exec.ExitError
		if errors.As(ge.Err, &ee) {
			return ee.ExitCode()
		}
	}
	return -1
}

var oidRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// IsOID reports whether s is a full hex object id.
func IsOID(s string) bool { return oidRe.MatchString(s) }

var refRe = regexp.MustCompile(`^[A-Za-z0-9._/@+-]{1,200}$`)

// SafeRevision reports whether s is safe to pass as a revision (no leading
// dash, no revision-range operators beyond what we allow).
func SafeRevision(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.Contains(s, "..") || strings.HasSuffix(s, ".lock") {
		return false
	}
	return refRe.MatchString(s)
}

// ResolveCommit resolves a revision to a full commit id.
func ResolveCommit(ctx context.Context, gitDir, rev string) (string, error) {
	if !SafeRevision(rev) {
		return "", fmt.Errorf("invalid revision")
	}
	return RunString(ctx, gitDir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
}

// RefExists reports whether a full ref name exists.
func RefTarget(ctx context.Context, gitDir, ref string) (string, bool) {
	out, err := RunString(ctx, gitDir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
	if err != nil || out == "" {
		return "", false
	}
	return out, true
}

// Ref is a ref name and its target.
type Ref struct {
	Name string
	OID  string
}

// ListRefs lists refs under the given prefixes.
func ListRefs(ctx context.Context, gitDir string, prefixes ...string) ([]Ref, error) {
	args := append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, prefixes...)
	out, err := Run(ctx, gitDir, nil, args...)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ' ')
		if i < 0 {
			continue
		}
		refs = append(refs, Ref{OID: line[:i], Name: line[i+1:]})
	}
	return refs, nil
}

// UpdateRef performs a compare-and-swap ref update. old may be "" to mean
// "must not exist".
func UpdateRef(ctx context.Context, gitDir, ref, newOID, oldOID string, env []string) error {
	if oldOID == "" {
		oldOID = ZeroOID
	}
	_, err := Run(ctx, gitDir, &RunOpts{Env: env}, "update-ref", "-m", "newgit", ref, newOID, oldOID)
	return err
}

// HashObject writes a blob and returns its id.
func HashObject(ctx context.Context, gitDir string, data []byte, env []string) (string, error) {
	out, err := Run(ctx, gitDir, &RunOpts{Stdin: bytes.NewReader(data), Env: env}, "hash-object", "-w", "--stdin")
	return strings.TrimSpace(string(out)), err
}

// MkTree builds a tree from "mode type oid\tname" entries.
func MkTree(ctx context.Context, gitDir string, entries []string, env []string) (string, error) {
	in := strings.Join(entries, "\n")
	if in != "" {
		in += "\n"
	}
	out, err := Run(ctx, gitDir, &RunOpts{Stdin: strings.NewReader(in), Env: env}, "mktree")
	return strings.TrimSpace(string(out)), err
}

// Identity is a git author/committer identity.
type Identity struct {
	Name  string
	Email string
}

// IdentityEnv returns env vars setting author and committer.
func IdentityEnv(id Identity) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + id.Name, "GIT_AUTHOR_EMAIL=" + id.Email,
		"GIT_COMMITTER_NAME=" + id.Name, "GIT_COMMITTER_EMAIL=" + id.Email,
	}
}

// CommitTree creates a commit object.
func CommitTree(ctx context.Context, gitDir, tree string, parents []string, msg string, id Identity, extraEnv []string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	env := append(IdentityEnv(id), extraEnv...)
	out, err := Run(ctx, gitDir, &RunOpts{Stdin: strings.NewReader(msg), Env: env}, args...)
	return strings.TrimSpace(string(out)), err
}

// CatBlob returns the contents of an object expression like "<commit>:<path>".
func CatBlob(ctx context.Context, gitDir, expr string) ([]byte, error) {
	return Run(ctx, gitDir, nil, "cat-file", "blob", expr)
}

// ObjectSize returns the size of an object.
func ObjectSize(ctx context.Context, gitDir, expr string) (int64, error) {
	s, err := RunString(ctx, gitDir, "cat-file", "-s", expr)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

// ObjectType returns the type of an object expression.
func ObjectType(ctx context.Context, gitDir, expr string) (string, error) {
	return RunString(ctx, gitDir, "cat-file", "-t", expr)
}

// Batch is a long-lived `git cat-file --batch` process for fast object reads.
type Batch struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// NewBatch starts a cat-file --batch process.
func NewBatch(ctx context.Context, gitDir string) (*Batch, error) {
	cmd := Cmd(ctx, gitDir, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	outp, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Batch{cmd: cmd, in: in, out: bufio.NewReaderSize(outp, 64*1024)}, nil
}

// ErrMissing is returned by Batch.Get for missing objects.
var ErrMissing = errors.New("object missing")

// Get reads one object. Returns type and content.
func (b *Batch) Get(expr string) (string, []byte, error) {
	if strings.ContainsAny(expr, "\n") {
		return "", nil, fmt.Errorf("bad expr")
	}
	if _, err := io.WriteString(b.in, expr+"\n"); err != nil {
		return "", nil, err
	}
	header, err := b.out.ReadString('\n')
	if err != nil {
		return "", nil, err
	}
	header = strings.TrimSuffix(header, "\n")
	if strings.HasSuffix(header, " missing") || strings.HasSuffix(header, " ambiguous") {
		return "", nil, ErrMissing
	}
	parts := strings.Fields(header)
	if len(parts) != 3 {
		return "", nil, fmt.Errorf("bad cat-file header %q", header)
	}
	size, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", nil, err
	}
	buf := make([]byte, size+1)
	if _, err := io.ReadFull(b.out, buf); err != nil {
		return "", nil, err
	}
	return parts[1], buf[:size], nil
}

// Close stops the batch process.
func (b *Batch) Close() {
	b.in.Close()
	_ = b.cmd.Wait()
}

// RunStringEnv runs git with extra env and returns trimmed stdout.
func RunStringEnv(ctx context.Context, gitDir string, env []string, args ...string) (string, error) {
	b, err := Run(ctx, gitDir, &RunOpts{Env: env}, args...)
	return strings.TrimSpace(string(b)), err
}
