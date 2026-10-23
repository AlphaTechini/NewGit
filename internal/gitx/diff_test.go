package gitx

import "testing"

func TestWordDiffSegments(t *testing.T) {
	out := ParseDiff("diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1,3 +1,3 @@\n keep\n-\tcount := 1 + foo(x)\n+\tcounter := 1 + foo(x)\n-totally different line here\n+zzz\n")
	if len(out) != 1 || out[0].Anchor == "" || out[0].Anchor[:5] != "diff-" {
		t.Fatalf("bad parse: %+v", out)
	}
	ls := out[0].Hunks[0].Lines
	if ls[1].Segments == nil || ls[2].Segments == nil {
		t.Fatalf("expected segments on similar pair: %+v", ls)
	}
	changed := func(s []Segment) string {
		r := ""
		for _, x := range s {
			if x.Changed {
				r += x.Text
			}
		}
		return r
	}
	if changed(ls[1].Segments) != "count" || changed(ls[2].Segments) != "counter" {
		t.Errorf("word snap: %q / %q", changed(ls[1].Segments), changed(ls[2].Segments))
	}
	if ls[3].Segments != nil || ls[4].Segments != nil {
		t.Errorf("dissimilar pair should have no segments")
	}
	var join string
	for _, x := range ls[1].Segments {
		join += x.Text
	}
	if join != ls[1].Content {
		t.Errorf("segments do not reassemble: %q vs %q", join, ls[1].Content)
	}
}
