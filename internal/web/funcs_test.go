package web

import (
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

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/a/b?x=1": "/a/b?x=1", "//evil.com": "/", "/\\evil.com": "/", "https://x": "/", "": "/", "/ok": "/ok"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q)=%q want %q", in, got, want)
		}
	}
}

func TestWithFlash(t *testing.T) {
	if got := withFlash("/a/b#latest", "commented"); got != "/a/b?flash=commented#latest" {
		t.Error(got)
	}
	if got := withFlash("/a?x=1", "closed"); got != "/a?x=1&flash=closed" {
		t.Error(got)
	}
}
