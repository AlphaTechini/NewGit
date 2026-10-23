package web

import (
	"bytes"
	"fmt"
	"html/template"
	"path"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	chtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/AlphaTechini/newgit/internal/gitx"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
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

var highlighter = chtml.New(chtml.WithClasses(true), chtml.WithLineNumbers(true), chtml.LineNumbersInTable(true), chtml.WithLinkableLineNumbers(true, "L"))

func chromaCSS() string {
	var buf bytes.Buffer
	style := styles.Get("github")
	_ = chtml.New(chtml.WithClasses(true), chtml.WithLineNumbers(true), chtml.LineNumbersInTable(true)).WriteCSS(&buf, style)
	return buf.String()
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
		"markdown":   renderMarkdown,
		"timeago":    timeAgo,
		"datetime":   func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"isodate":    func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
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
