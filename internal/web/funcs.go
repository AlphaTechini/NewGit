package web

import (
	"bytes"
	"fmt"
	"html/template"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	chtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/AlphaTechini/newgit/internal/gitx"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(gmhtml.WithHardWraps()),
	// Raw HTML is not rendered (goldmark's default is safe).
)

func renderMarkdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String())
}

// highlighter emits inline line numbers: every <span class="line"> contains
// its own <span class="ln" id="L12"><a class="lnlinks" href="#L12">12</a></span>
// followed by <span class="cl">code</span>.
var highlighter = chtml.New(chtml.WithClasses(true), chtml.WithLineNumbers(true), chtml.WithLinkableLineNumbers(true, "L"))

// chromaCSS returns the syntax-highlighting stylesheet: the `github` palette
// unscoped (light), and `github-dark` scoped to dark mode, both for the
// system preference (unless data-theme="light") and for data-theme="dark".
// Chroma's own page background is removed so the page surface shows through.
// Selectors that exist only in the light palette are reset (`unset`) in the
// dark scopes so a light-only colour can never leak into dark mode.
func chromaCSS() string {
	f := chtml.New(chtml.WithClasses(true), chtml.WithLineNumbers(true))
	var lb, db bytes.Buffer
	_ = f.WriteCSS(&lb, styles.Get("github"))
	_ = f.WriteCSS(&db, styles.Get("github-dark"))
	light, dark := parseChromaCSS(lb.String()), parseChromaCSS(db.String())
	inDark := map[string]bool{}
	for _, r := range dark {
		inDark[r.sel] = true
	}
	for _, r := range light {
		if !inDark[r.sel] {
			dark = append(dark, chromaRule{r.sel, resetDecls(r.body)})
		}
	}
	var out strings.Builder
	write := func(rules []chromaRule, prefix string) {
		for _, r := range rules {
			out.WriteString(prefix + r.sel + " { " + r.body + " }\n")
		}
	}
	write(light, "")
	out.WriteString("@media (prefers-color-scheme: dark) {\n")
	write(dark, `:root:not([data-theme="light"]) `)
	out.WriteString("}\n")
	write(dark, `:root[data-theme="dark"] `)
	return out.String()
}

type chromaRule struct{ sel, body string }

var bgDeclRe = regexp.MustCompile(`\s*background-color:\s*[^;]+;?`)

// parseChromaCSS reads chroma's one-rule-per-line CSS, dropping comments and
// the .bg rule, and stripping the background from the bare .chroma rule.
func parseChromaCSS(css string) []chromaRule {
	var rules []chromaRule
	for _, line := range strings.Split(css, "\n") {
		if i := strings.Index(line, "*/"); i >= 0 {
			line = line[i+2:]
		}
		line = strings.TrimSpace(line)
		open, closing := strings.Index(line, "{"), strings.LastIndex(line, "}")
		if open < 0 || closing < open {
			continue
		}
		sel, body := strings.TrimSpace(line[:open]), strings.TrimSpace(line[open+1:closing])
		switch sel {
		case ".bg":
			continue
		case ".chroma":
			body = strings.TrimSpace(bgDeclRe.ReplaceAllString(body, ""))
		}
		rules = append(rules, chromaRule{sel, body})
	}
	return rules
}

// resetDecls turns "color: #fff; font-weight: bold" into
// "color: unset; font-weight: unset".
func resetDecls(body string) string {
	var out []string
	for _, d := range strings.Split(body, ";") {
		if prop, _, ok := strings.Cut(d, ":"); ok {
			out = append(out, strings.TrimSpace(prop)+": unset")
		}
	}
	return strings.Join(out, "; ")
}

func highlight(filename, src string) template.HTML {
	lexer := lexers.Match(path.Base(filename))
	if lexer == nil {
		lexer = lexers.Analyse(src)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	it, err := lexer.Tokenise(nil, src)
	if err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(src) + "</pre>")
	}
	var buf bytes.Buffer
	if err := highlighter.Format(&buf, styles.Get("github"), it); err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(src) + "</pre>")
	}
	return template.HTML(buf.String())
}

func timeAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month") + " ago"
	default:
		return plural(int(d.Hours()/24/365), "year") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func humanSize(n int64) string {
	switch {
	case n < 0:
		return ""
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/1024/1024)
	}
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func initials(s string) string {
	if s == "" {
		return "?"
	}
	return strings.ToUpper(s[:1])
}

// avatarHue gives each name a stable colour.
func avatarHue(s string) int {
	h := 0
	for _, c := range s {
		h = (h*31 + int(c)) % 360
	}
	return h
}

func funcMap(s *Server) template.FuncMap {
	return template.FuncMap{
		"markdown": renderMarkdown,
		"timeago":  timeAgo,
		"datetime": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"isodate":  func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"iso":      func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"day":      func(t time.Time) string { return t.UTC().Format("Jan 2, 2006") },
		"pct": func(a, b int) int {
			if b <= 0 {
				return 0
			}
			return a * 100 / b
		},
		"diffstat":   diffStat,
		"short":      short,
		"size":       humanSize,
		"initials":   initials,
		"hue":        avatarHue,
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"pathjoin":   func(a, b string) string { return strings.Trim(a+"/"+b, "/") },
		"split":      strings.Split,
		"hasPrefix":  strings.HasPrefix,
		"trimPrefix": strings.TrimPrefix,
		"lower":      strings.ToLower,
		"firstLine":  func(s string) string { return strings.SplitN(s, "\n", 2)[0] },
		"isZeroOID":  func(s string) bool { return s == gitx.ZeroOID },
		"diffClass": func(k byte) string {
			switch k {
			case '+':
				return "add"
			case '-':
				return "del"
			}
			return "ctx"
		},
		"lineNo": func(n int) string {
			if n == 0 {
				return ""
			}
			return fmt.Sprint(n)
		},
		"crumbs": func(p string) []crumb {
			var out []crumb
			if p == "" {
				return out
			}
			acc := ""
			for _, seg := range strings.Split(p, "/") {
				acc = strings.Trim(acc+"/"+seg, "/")
				out = append(out, crumb{Name: seg, Path: acc})
			}
			return out
		},
		"seq": func(n int) []int {
			out := make([]int, n)
			for i := range out {
				out[i] = i + 1
			}
			return out
		},
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
	}
}

type crumb struct {
	Name string
	Path string
}

// DiffStat is the aggregate of a []gitx.FileDiff (template func `diffstat`).
type DiffStat struct {
	Files     int
	Additions int
	Deletions int
}

func diffStat(files []gitx.FileDiff) DiffStat {
	st := DiffStat{Files: len(files)}
	for _, f := range files {
		st.Additions += f.Additions
		st.Deletions += f.Deletions
	}
	return st
}
