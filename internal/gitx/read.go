package gitx

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TreeEntry is one entry of a tree listing.
type TreeEntry struct {
	Mode string
	Type string // blob, tree, commit
	OID  string
	Size int64 // -1 for trees
	Name string
	Path string
}

// ListTree lists a directory at a commit. path "" is the root.
func ListTree(ctx context.Context, gitDir, commit, path string) ([]TreeEntry, error) {
	expr := commit + "^{tree}"
	if path != "" {
		expr = commit + ":" + path
	}
	out, err := Run(ctx, gitDir, nil, "ls-tree", "-z", "--long", "--end-of-options", expr)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		s := string(rec)
		tab := strings.IndexByte(s, '\t')
		if tab < 0 {
			continue
		}
		meta := strings.Fields(s[:tab])
		if len(meta) < 4 {
			continue
		}
		e := TreeEntry{Mode: meta[0], Type: meta[1], OID: meta[2], Name: s[tab+1:], Size: -1}
		if meta[3] != "-" {
			e.Size, _ = strconv.ParseInt(meta[3], 10, 64)
		}
		if path == "" {
			e.Path = e.Name
		} else {
			e.Path = path + "/" + e.Name
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if (entries[i].Type == "tree") != (entries[j].Type == "tree") {
			return entries[i].Type == "tree"
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

// Commit describes a commit.
type Commit struct {
	OID            string
	Parents        []string
	AuthorName     string
	AuthorEmail    string
	AuthorDate     time.Time
	CommitterName  string
	CommitterEmail string
	CommitDate     time.Time
	Subject        string
	Body           string
}

// Short returns the abbreviated id.
func (c Commit) Short() string {
	if len(c.OID) > 8 {
		return c.OID[:8]
	}
	return c.OID
}

const logFormat = "%H%x00%P%x00%an%x00%ae%x00%at%x00%cn%x00%ce%x00%ct%x00%s%x00%b%x1e"

func parseLog(out []byte) []Commit {
	var commits []Commit
	for _, rec := range bytes.Split(out, []byte{0x1e}) {
		s := strings.TrimLeft(string(rec), "\n")
		if s == "" {
			continue
		}
		f := strings.SplitN(s, "\x00", 10)
		if len(f) < 10 {
			continue
		}
		at, _ := strconv.ParseInt(f[4], 10, 64)
		ct, _ := strconv.ParseInt(f[7], 10, 64)
		c := Commit{
			OID: f[0], AuthorName: f[2], AuthorEmail: f[3], AuthorDate: time.Unix(at, 0),
			CommitterName: f[5], CommitterEmail: f[6], CommitDate: time.Unix(ct, 0),
			Subject: f[8], Body: strings.TrimSpace(f[9]),
		}
		if f[1] != "" {
			c.Parents = strings.Fields(f[1])
		}
		commits = append(commits, c)
	}
	return commits
}

// Log lists commits reachable from rev (optionally limited to path).
func Log(ctx context.Context, gitDir, rev, path string, skip, limit int) ([]Commit, error) {
	args := []string{"log", "--format=" + logFormat, "-n", strconv.Itoa(limit), "--skip", strconv.Itoa(skip), "--end-of-options", rev}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := Run(ctx, gitDir, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}

// LogRange lists commits in a range expression list (e.g. base..head), oldest first.
func LogRange(ctx context.Context, gitDir string, revs ...string) ([]Commit, error) {
	args := append([]string{"log", "--reverse", "--format=" + logFormat, "--end-of-options"}, revs...)
	out, err := Run(ctx, gitDir, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}

// GetCommit reads one commit.
func GetCommit(ctx context.Context, gitDir, rev string) (Commit, error) {
	out, err := Run(ctx, gitDir, nil, "log", "-1", "--format="+logFormat, "--end-of-options", rev)
	if err != nil {
		return Commit{}, err
	}
	cs := parseLog(out)
	if len(cs) == 0 {
		return Commit{}, fmt.Errorf("commit not found")
	}
	return cs[0], nil
}

// Branch is a branch with its tip commit.
type Branch struct {
	Name   string
	Commit Commit
	Ahead  int
	Behind int
}

// ListBranches lists branches sorted by most recent commit.
func ListBranches(ctx context.Context, gitDir string) ([]Branch, error) {
	refs, err := ReadRefs(ctx, gitDir, "refs/heads/")
	return refs.Branches, err
}

// AheadBehind returns how many commits a is ahead of and behind b.
func AheadBehind(ctx context.Context, gitDir, a, b string) (int, int, error) {
	out, err := RunString(ctx, gitDir, "rev-list", "--left-right", "--count", "--end-of-options", a+"..."+b)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("bad rev-list output")
	}
	ahead, _ := strconv.Atoi(f[0])
	behind, _ := strconv.Atoi(f[1])
	return ahead, behind, nil
}

// MergeBase returns the merge base of two commits.
func MergeBase(ctx context.Context, gitDir, a, b string) (string, error) {
	return RunString(ctx, gitDir, "merge-base", "--end-of-options", a, b)
}

// IsAncestor reports whether a is an ancestor of b.
func IsAncestor(ctx context.Context, gitDir, a, b string) bool {
	_, err := Run(ctx, gitDir, nil, "merge-base", "--is-ancestor", "--end-of-options", a, b)
	return err == nil
}

// RevList returns commit ids reachable from include but not from exclude.
func RevList(ctx context.Context, gitDir string, include, exclude []string) ([]string, error) {
	if len(include) == 0 {
		return nil, nil
	}
	args := []string{"rev-list", "--stdin"}
	var in strings.Builder
	for _, r := range include {
		in.WriteString(r + "\n")
	}
	for _, r := range exclude {
		in.WriteString("^" + r + "\n")
	}
	out, err := Run(ctx, gitDir, &RunOpts{Stdin: strings.NewReader(in.String())}, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// MergeResult is the outcome of a trial merge.
type MergeResult struct {
	Tree      string
	Clean     bool
	Conflicts []string
}

// MergeTree performs a merge without a worktree (git >= 2.38).
func MergeTree(ctx context.Context, gitDir, ours, theirs string) (MergeResult, error) {
	out, err := Run(ctx, gitDir, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", "--end-of-options", ours, theirs)
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if err != nil {
		if ExitCode(err) == 1 && len(lines) > 0 && IsOID(lines[0]) {
			res := MergeResult{Tree: lines[0], Clean: false}
			seen := map[string]bool{}
			for _, l := range lines[1:] {
				if l != "" && !seen[l] {
					seen[l] = true
					res.Conflicts = append(res.Conflicts, l)
				}
			}
			return res, nil
		}
		return MergeResult{}, err
	}
	return MergeResult{Tree: strings.TrimSpace(lines[0]), Clean: true}, nil
}

// RangeDiff runs git range-diff between two ranges and returns plain output.
func RangeDiff(ctx context.Context, gitDir, base1, tip1, base2, tip2 string) (string, error) {
	// range-diff does not accept --end-of-options (git 2.43), so only ever
	// pass it full object ids.
	for _, oid := range []string{base1, tip1, base2, tip2} {
		if !IsOID(oid) {
			return "", fmt.Errorf("range-diff needs full object ids")
		}
	}
	out, err := Run(ctx, gitDir, nil, "range-diff", "--no-color", base1+".."+tip1, base2+".."+tip2)
	return string(out), err
}

// TagInfo is a tag with the commit it points at.
type TagInfo struct {
	Name    string
	OID     string // commit the tag points at (annotated tags are peeled)
	Subject string // tag message subject (annotated) or commit subject (lightweight)
	Date    time.Time
}

// ListTags lists tags, newest first by creator date.
func ListTags(ctx context.Context, gitDir string) ([]TagInfo, error) {
	refs, err := ReadRefs(ctx, gitDir, "refs/tags/")
	return refs.Tags, err
}

// CountCommits counts commits in a rev-list expression list (e.g. "main", or
// "head", "^base"). Returns 0 and an error when a revision does not resolve.
func CountCommits(ctx context.Context, gitDir string, revs ...string) (int, error) {
	args := append([]string{"rev-list", "--count", "--end-of-options"}, revs...)
	out, err := RunString(ctx, gitDir, args...)
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n, nil
}

// LogRangeLimit is like LogRange but returns at most limit of the newest
// commits (still ordered oldest first).
func LogRangeLimit(ctx context.Context, gitDir string, limit int, revs ...string) ([]Commit, error) {
	args := append([]string{"log", "--reverse", "-n", strconv.Itoa(limit), "--format=" + logFormat, "--end-of-options"}, revs...)
	out, err := Run(ctx, gitDir, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}
