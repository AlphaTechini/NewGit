package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Refs is a snapshot of a repository's branches and tags, read with one git
// call. Process start-up dominates the cost of small git commands (tens of ms
// on Windows), so pages read this once instead of asking git ref by ref.
type Refs struct {
	Branches []Branch  // most recent commit first
	Tags     []TagInfo // newest first
	commits  map[string]string
}

// Commit returns the commit a full ref name (refs/heads/x, refs/tags/x)
// points at, with annotated tags peeled. Refs to non-commits are not found.
func (r Refs) Commit(ref string) (string, bool) {
	oid, ok := r.commits[ref]
	return oid, ok
}

// ReadRefs lists branches and tags. Without prefixes it reads both.
func ReadRefs(ctx context.Context, gitDir string, prefixes ...string) (Refs, error) {
	if len(prefixes) == 0 {
		prefixes = []string{"refs/heads/", "refs/tags/"}
	}
	// creatordate is the committer date for commits and the tagger date for
	// annotated tags, so one sort key orders both lists.
	args := append([]string{"for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%00%(objectname)%00%(objecttype)%00%(*objectname)%00%(*objecttype)%00%(subject)%00%(authorname)%00%(creatordate:unix)"},
		prefixes...)
	out, err := Run(ctx, gitDir, nil, args...)
	if err != nil {
		return Refs{}, err
	}
	refs := Refs{commits: map[string]string{}}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) < 8 {
			continue
		}
		name, oid, typ, peeled, peeledType := f[0], f[1], f[2], f[3], f[4]
		ts, _ := strconv.ParseInt(f[7], 10, 64)
		date := time.Unix(ts, 0)
		switch {
		case typ == "commit":
			refs.commits[name] = oid
		case peeledType == "commit":
			refs.commits[name] = peeled
		}
		if short, ok := strings.CutPrefix(name, "refs/heads/"); ok {
			refs.Branches = append(refs.Branches, Branch{Name: short, Commit: Commit{OID: oid, Subject: f[5], AuthorName: f[6], CommitDate: date}})
		} else if short, ok := strings.CutPrefix(name, "refs/tags/"); ok {
			if peeled != "" {
				oid = peeled
			}
			refs.Tags = append(refs.Tags, TagInfo{Name: short, OID: oid, Subject: f[5], Date: date})
		}
	}
	return refs, nil
}

// HeadBranch returns the branch HEAD points at. It reads the HEAD file of the
// bare repository directly and only asks git when the file is not a plain
// symref (for example the placeholder the reftable backend writes).
func HeadBranch(ctx context.Context, gitDir string) string {
	if b, err := os.ReadFile(filepath.Join(gitDir, "HEAD")); err == nil {
		if name, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/"); ok && name != "" && name != ".invalid" {
			return name
		}
	}
	s, err := RunString(ctx, gitDir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "main"
	}
	return s
}

// ObjectInfo returns the type and size of an object expression in one call.
func ObjectInfo(ctx context.Context, gitDir, expr string) (typ string, size int64, err error) {
	if strings.ContainsAny(expr, "\n") {
		return "", 0, ErrMissing
	}
	out, err := Run(ctx, gitDir, &RunOpts{Stdin: strings.NewReader(expr + "\n")}, "cat-file", "--batch-check=%(objecttype) %(objectsize)")
	if err != nil {
		return "", 0, err
	}
	// A missing object prints "<expr> missing", which never parses as a size.
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) != 2 {
		return "", 0, ErrMissing
	}
	size, err = strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return "", 0, ErrMissing
	}
	return f[0], size, nil
}
