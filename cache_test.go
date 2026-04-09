package ociconfigs

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheManagerIsStaleNoManifest(t *testing.T) {
	cm := &cacheManager{dir: t.TempDir()}
	if !cm.isStale(time.Hour) {
		t.Fatal("expected stale when no manifest exists")
	}
}

func TestCacheManagerIsStaleAfterWrite(t *testing.T) {
	cm := &cacheManager{dir: t.TempDir()}
	if err := cm.writeManifest("sha256:abc"); err != nil {
		t.Fatal(err)
	}
	if cm.isStale(time.Hour) {
		t.Fatal("expected fresh immediately after write")
	}
}

func TestCacheManagerIsStaleExpired(t *testing.T) {
	dir := t.TempDir()
	cm := &cacheManager{dir: dir}

	past := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	rec := `{"digest":"sha256:abc","cached_at":"` + past + `"}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}

	if !cm.isStale(24 * time.Hour) {
		t.Fatal("expected stale after TTL exceeded")
	}
}

func TestCacheManagerDigestRoundtrip(t *testing.T) {
	cm := &cacheManager{dir: t.TempDir()}

	if got := cm.digest(); got != "" {
		t.Fatalf("digest() = %q before write, want empty", got)
	}

	const want = "sha256:deadbeef"
	if err := cm.writeManifest(want); err != nil {
		t.Fatal(err)
	}
	if got := cm.digest(); got != want {
		t.Fatalf("digest() = %q, want %q", got, want)
	}
}

func TestCacheManagerOpenFile(t *testing.T) {
	dir := t.TempDir()
	cm := &cacheManager{dir: dir}

	const content = "hello, cache"
	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	rc, err := cm.openFile("test.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("got %q, want %q", got, content)
	}
}

func TestCacheManagerAllFilesCached(t *testing.T) {
	dir := t.TempDir()
	cm := &cacheManager{dir: dir}

	files := []FileSpec{
		{Name: "schema", Path: "schema.gql"},
		{Name: "config", Path: "config.yaml"},
	}

	if cm.allFilesCached(files) {
		t.Fatal("expected false with no files present")
	}

	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.Path), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if !cm.allFilesCached(files) {
		t.Fatal("expected true after all files written")
	}
}

func TestCacheManagerHasFile(t *testing.T) {
	dir := t.TempDir()
	cm := &cacheManager{dir: dir}

	if cm.hasFile("missing.txt") {
		t.Fatal("expected false for absent file")
	}

	if err := os.WriteFile(filepath.Join(dir, "present.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !cm.hasFile("present.txt") {
		t.Fatal("expected true for present file")
	}
}

func TestNewCacheManagerKeyEncoding(t *testing.T) {
	cm1 := newCacheManager("/cache", "registry.example.com", "org/repo", "latest")
	cm2 := newCacheManager("/cache", "registry.example.com", "org/repo", "v1.0")
	if cm1.dir == cm2.dir {
		t.Fatal("different tags must produce different cache dirs")
	}
}
