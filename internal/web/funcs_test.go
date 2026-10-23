package web

import (
	"regexp"
	"strings"
	"testing"
)

func TestChromaCSS(t *testing.T) {
	css := chromaCSS()
	for _, want := range []string{
		".chroma .k {",
		"@media (prefers-color-scheme: dark) {",
		`:root:not([data-theme="light"]) .chroma .k {`,
		`:root[data-theme="dark"] .chroma .k {`,
		`:root:not([data-theme="light"]) .chroma .p { color: unset }`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(css, ".bg ") {
		t.Error(".bg rule should be dropped")
	}
	for _, l := range strings.Split(css, "\n") {
		if strings.Contains(l, ".chroma {") && strings.Contains(l, "background-color") {
			t.Errorf("bare .chroma keeps background: %s", l)
		}
	}
}

// Comments keep a line break for every newline; repository Markdown files
// let wrapped lines flow as one paragraph (CommonMark soft breaks).
func TestHardWrapsOnlyForUserText(t *testing.T) {
	const src = "line one\nline two"
	if got := string(renderMarkdown(src)); !strings.Contains(got, "<br>") {
		t.Errorf("comment path: want <br> for a soft break, got %q", got)
	}
	if got := string(renderMarkdownDoc(src)); strings.Contains(got, "<br") {
		t.Errorf("doc path: want no <br> for a soft break, got %q", got)
	}
}

func TestSafeNext(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b?x=1#y", "/a/b?x=1#y"},
		{"/", "/"},
		{"/ok", "/ok"},
		{"//evil.com", "/"},
		{"/\\evil.com", "/"},
		{"\\\\evil.com", "/"},
		{"/\t/evil.com", "/"},   // decoded form of /%09/evil.com
		{"/\x0b/evil.com", "/"}, // vertical tab
		{"/\r/evil.com", "/"},
		{"/\n/evil.com", "/"},
		{"/\x00/x", "/"},
		{"/\x7f/x", "/"},
		{"/%09/evil.com", "/%09/evil.com"}, // literal percent-encoding is inert in a Location header
		{"https://evil.com", "/"},
		{"HTTPS://evil.com", "/"},
		{"/\\\\evil.com", "/"},
		{"", "/"},
		{"evil.com", "/"},
		{"//", "/"},
	}
	for _, c := range cases {
		if got := safeNext(c.in); got != c.want {
			t.Errorf("safeNext(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestWithFlash(t *testing.T) {
	if got := withFlash("/a/b#ng:latest", "commented"); got != "/a/b?flash=commented#ng:latest" {
		t.Error(got)
	}
	if got := withFlash("/a?x=1", "closed"); got != "/a?x=1&flash=closed" {
		t.Error(got)
	}
}

// Redirects target the page's own anchor, ng:latest. Markdown headings get
// auto ids from goldmark (WithAutoHeadingID) that only ever contain [a-z0-9-],
// so a comment heading can never shadow it.
func TestHeadingIDsCannotShadowAnchor(t *testing.T) {
	out := string(renderMarkdown("# Latest\n\n## ng:latest\n\n### Ng: latest ✓\n\n# !!!\n\nSetext\n------\n"))
	if strings.Contains(out, `id="ng:latest"`) {
		t.Fatalf("markdown produced the reserved anchor: %s", out)
	}
	if !strings.Contains(out, `id="latest"`) {
		t.Fatalf("expected auto id for '# Latest' in: %s", out)
	}
	for _, m := range regexp.MustCompile(`id="([^"]*)"`).FindAllStringSubmatch(out, -1) {
		if strings.ContainsRune(m[1], ':') {
			t.Errorf("auto heading id %q contains ':'", m[1])
		}
	}
}
