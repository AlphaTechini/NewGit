package gitx

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DiffLine is one line of a unified diff hunk.
type DiffLine struct {
	Kind    byte // ' ', '+', '-'
	OldNo   int
	NewNo   int
	Content string
	// Segments splits Content into unchanged/changed runs for word-level
	// (intraline) highlighting. It is nil unless this line is part of a
	// similar -/+ pair; templates render Segments when non-nil, else Content.
	Segments []Segment
}

// Segment is a run of text inside a changed line.
type Segment struct {
	Text    string
	Changed bool
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
	// Anchor is a stable DOM id for the file: "diff-" + 8 hex chars of
	// sha1(path), where path is OldPath for deleted files, else NewPath.
	Anchor string
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
	files := []FileDiff{}
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
	for i := range files {
		f := &files[i]
		p := f.NewPath
		if f.Status == "deleted" {
			p = f.OldPath
		}
		sum := sha1.Sum([]byte(p))
		f.Anchor = "diff-" + hex.EncodeToString(sum[:4])
		for j := range f.Hunks {
			annotateWords(&f.Hunks[j])
		}
	}
	return files
}

const (
	maxWordDiffLine    = 500 // lines this long (in runes) are not diffed
	minWordDiffSimilar = 30  // percent of the longer line that must be unchanged
)

// annotateWords pairs each run of deleted lines with the run of added lines
// that directly follows it (line i with line i) and fills in Segments.
func annotateWords(h *Hunk) {
	ls := h.Lines
	for i := 0; i < len(ls); {
		if ls[i].Kind != '-' {
			i++
			continue
		}
		d0 := i
		for i < len(ls) && ls[i].Kind == '-' {
			i++
		}
		a0 := i
		for i < len(ls) && ls[i].Kind == '+' {
			i++
		}
		nd, na := a0-d0, i-a0
		for k := 0; k < nd && k < na; k++ {
			old, nw := wordSegments(ls[d0+k].Content, ls[a0+k].Content)
			ls[d0+k].Segments, ls[a0+k].Segments = old, nw
		}
	}
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordSegments computes the changed middle of two similar lines.
func wordSegments(a, b string) (sa, sb []Segment) {
	if utf8.RuneCountInString(a) >= maxWordDiffLine || utf8.RuneCountInString(b) >= maxWordDiffLine {
		return nil, nil
	}
	ra, rb := []rune(a), []rune(b)
	minLen := len(ra)
	if len(rb) < minLen {
		minLen = len(rb)
	}
	p := 0
	for p < minLen && ra[p] == rb[p] {
		p++
	}
	s := 0
	for s < minLen-p && ra[len(ra)-1-s] == rb[len(rb)-1-s] {
		s++
	}
	// Snap to word boundaries so "counter" vs "count" highlights whole words.
	for p > 0 && isWordRune(ra[p-1]) && ((p < len(ra) && isWordRune(ra[p])) || (p < len(rb) && isWordRune(rb[p]))) {
		p--
	}
	for s > 0 && isWordRune(ra[len(ra)-s]) && ((len(ra)-s-1 >= 0 && isWordRune(ra[len(ra)-s-1])) || (len(rb)-s-1 >= 0 && isWordRune(rb[len(rb)-s-1]))) {
		s--
	}
	longer := len(ra)
	if len(rb) > longer {
		longer = len(rb)
	}
	if longer == 0 || (p+s)*100 < minWordDiffSimilar*longer || (p == len(ra) && p == len(rb)) {
		return nil, nil
	}
	return splitSegments(ra, p, s), splitSegments(rb, p, s)
}

func splitSegments(r []rune, p, s int) []Segment {
	var out []Segment
	if p > 0 {
		out = append(out, Segment{Text: string(r[:p])})
	}
	if mid := r[p : len(r)-s]; len(mid) > 0 {
		out = append(out, Segment{Text: string(mid), Changed: true})
	}
	if s > 0 {
		out = append(out, Segment{Text: string(r[len(r)-s:])})
	}
	return out
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
