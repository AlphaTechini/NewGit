package gitx

import (
	"context"
	"strconv"
	"strings"
)

// DiffLine is one line of a unified diff hunk.
type DiffLine struct {
	Kind    byte // ' ', '+', '-'
	OldNo   int
	NewNo   int
	Content string
}

// Hunk is a diff hunk.
type Hunk struct {
	Header string
	Lines  []DiffLine
}

// FileDiff is the diff of one file.
type FileDiff struct {
	OldPath   string
	NewPath   string
	Status    string // added, deleted, modified, renamed
	Binary    bool
	Hunks     []Hunk
	Additions int
	Deletions int
	Truncated bool
}

// Path returns the display path.
func (f FileDiff) Path() string {
	if f.Status == "deleted" {
		return f.OldPath
	}
	if f.Status == "renamed" {
		return f.OldPath + " → " + f.NewPath
	}
	return f.NewPath
}

const maxDiffLinesPerFile = 3000

// ParseDiff parses `git diff` unified output.
func ParseDiff(out string) []FileDiff {
	var files []FileDiff
	var cur *FileDiff
	var hunk *Hunk
	oldNo, newNo := 0, 0
	flush := func() {
		if cur != nil {
			if hunk != nil {
				cur.Hunks = append(cur.Hunks, *hunk)
				hunk = nil
			}
			files = append(files, *cur)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &FileDiff{Status: "modified"}
			rest := strings.TrimPrefix(line, "diff --git ")
			if i := strings.Index(rest, " b/"); i >= 0 && strings.HasPrefix(rest, "a/") {
				cur.OldPath = rest[2:i]
				cur.NewPath = rest[i+3:]
			}
		case cur == nil:
			continue
		case hunk == nil && strings.HasPrefix(line, "new file mode"):
			cur.Status = "added"
		case hunk == nil && strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "deleted"
		case hunk == nil && strings.HasPrefix(line, "rename from "):
			cur.Status = "renamed"
			cur.OldPath = strings.TrimPrefix(line, "rename from ")
		case hunk == nil && strings.HasPrefix(line, "rename to "):
			cur.NewPath = strings.TrimPrefix(line, "rename to ")
		case hunk == nil && strings.HasPrefix(line, "Binary files "):
			cur.Binary = true
		case hunk == nil && strings.HasPrefix(line, "--- "):
			if p := strings.TrimPrefix(line, "--- "); strings.HasPrefix(p, "a/") {
				cur.OldPath = p[2:]
			}
		case hunk == nil && strings.HasPrefix(line, "+++ "):
			if p := strings.TrimPrefix(line, "+++ "); strings.HasPrefix(p, "b/") {
				cur.NewPath = p[2:]
			}
		case strings.HasPrefix(line, "@@"):
			if hunk != nil {
				cur.Hunks = append(cur.Hunks, *hunk)
			}
			hunk = &Hunk{Header: line}
			oldNo, newNo = parseHunkHeader(line)
		case hunk != nil:
			if cur.Truncated {
				continue
			}
			if len(line) == 0 {
				continue
			}
			k := line[0]
			c := line[1:]
			switch k {
			case '+':
				hunk.Lines = append(hunk.Lines, DiffLine{Kind: '+', NewNo: newNo, Content: c})
				newNo++
				cur.Additions++
			case '-':
				hunk.Lines = append(hunk.Lines, DiffLine{Kind: '-', OldNo: oldNo, Content: c})
				oldNo++
				cur.Deletions++
			case ' ':
				hunk.Lines = append(hunk.Lines, DiffLine{Kind: ' ', OldNo: oldNo, NewNo: newNo, Content: c})
				oldNo++
				newNo++
			case '\\':
				// "\ No newline at end of file"
			}
			n := 0
			for _, h := range cur.Hunks {
				n += len(h.Lines)
			}
			if n+len(hunk.Lines) > maxDiffLinesPerFile {
				cur.Truncated = true
			}
		}
	}
	flush()
	return files
}

func parseHunkHeader(h string) (int, int) {
	// @@ -a,b +c,d @@
	f := strings.Fields(h)
	if len(f) < 3 {
		return 0, 0
	}
	num := func(s string) int {
		s = strings.TrimLeft(s, "-+")
		if i := strings.IndexByte(s, ','); i >= 0 {
			s = s[:i]
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	return num(f[1]), num(f[2])
}

// DiffCommits returns the parsed diff between two commits.
func DiffCommits(ctx context.Context, gitDir, from, to string) ([]FileDiff, error) {
	out, err := Run(ctx, gitDir, nil, "diff", "--no-color", "--no-ext-diff", "-M", "--end-of-options", from, to)
	if err != nil {
		return nil, err
	}
	return ParseDiff(string(out)), nil
}

// CommitDiff returns the diff a commit introduced (against its first parent).
func CommitDiff(ctx context.Context, gitDir string, c Commit) ([]FileDiff, error) {
	if len(c.Parents) == 0 {
		out, err := Run(ctx, gitDir, nil, "diff-tree", "-p", "--no-color", "--no-ext-diff", "-M", "--root", "-r", "--end-of-options", c.OID)
		if err != nil {
			return nil, err
		}
		return ParseDiff(string(out)), nil
	}
	return DiffCommits(ctx, gitDir, c.Parents[0], c.OID)
}
