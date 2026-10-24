package gitx

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
)

func newBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

func TestCatBlobHead(t *testing.T) {
	dir := newBareRepo(t)
	ctx := context.Background()
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	bigOID, err := HashObject(ctx, dir, big, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CatBlobHead(ctx, dir, bigOID, 512)
	if err != nil || !bytes.Equal(got, big[:512]) {
		t.Fatalf("head of big blob: len=%d err=%v", len(got), err)
	}
	small, _ := HashObject(ctx, dir, []byte("hi"), nil)
	if got, err := CatBlobHead(ctx, dir, small, 512); err != nil || string(got) != "hi" {
		t.Fatalf("small blob: %q %v", got, err)
	}
	if got, err := CatBlobHead(ctx, dir, ZeroOID, 512); err == nil {
		t.Fatalf("missing object should error, got %q", got)
	}
}

func TestRunLimitedTruncates(t *testing.T) {
	dir := newBareRepo(t)
	ctx := context.Background()
	oid, _ := HashObject(ctx, dir, bytes.Repeat([]byte("x"), 100000), nil)
	out, truncated, err := RunLimited(ctx, dir, 100, "cat-file", "blob", oid)
	if err != nil || !truncated || len(out) != 100 {
		t.Fatalf("len=%d truncated=%v err=%v", len(out), truncated, err)
	}
	out, truncated, err = RunLimited(ctx, dir, 200000, "cat-file", "blob", oid)
	if err != nil || truncated || len(out) != 100000 {
		t.Fatalf("len=%d truncated=%v err=%v", len(out), truncated, err)
	}
}

func TestCutAtFileBoundary(t *testing.T) {
	two := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\ndiff --git a/y b/y\n--- a/y\n+++ b/y\n@@ -1 +1 @@\n-c\n+d\n"
	partial := two + "diff --git a/z b/z\n--- a/z\n+++ b/z\n@@ -1 +1"
	if got := string(cutAtFileBoundary([]byte(partial))); got != two {
		t.Fatalf("cut kept %q", got)
	}
	files := ParseDiff(string(cutAtFileBoundary([]byte(partial))))
	if len(files) != 2 || files[1].NewPath != "y" {
		t.Fatalf("parsed %+v", files)
	}
	if got := cutAtFileBoundary([]byte("diff --git a/x b/x\n+partial line")); got != nil {
		t.Fatalf("single partial file should be dropped, got %q", got)
	}
}
