package ociconfigs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testFiles is a representative set of FileSpecs used across client tests.
var testFiles = []FileSpec{
	{Name: "schema", Path: "schema.gql", MediaType: "application/graphql+text"},
	{Name: "persisted-op-manifest", Path: "persisted-op-manifest.json", MediaType: "application/json"},
	{Name: "ast", Path: "ast.dat", MediaType: "application/octet-stream"},
	{Name: "config", Path: "config.yaml", MediaType: "application/yaml"},
}

// makeClient returns a Client backed by a temp cache dir with injectable
// fetchDigest and download functions.
func makeClient(t *testing.T, fetchDigest func(context.Context, Config) (string, error), download func(context.Context, Config, *cacheManager) error) *Client {
	t.Helper()
	cfg := Config{
		Registry:   "registry.example.com",
		Repository: "myorg/configs",
		Tag:        "latest",
		Files:      testFiles,
		CacheDir:   t.TempDir(),
		MaxAge:     time.Hour,
	}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	cm := newCacheManager(cfg.CacheDir, cfg.Registry, cfg.Repository, cfg.Tag)
	return &Client{
		cfg:         cfg,
		cm:          cm,
		fetchDigest: fetchDigest,
		download:    download,
	}
}

// seedCache writes fake content for every FileSpec and a manifest record.
func seedCache(t *testing.T, cm *cacheManager, digest string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(cm.cacheDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range testFiles {
		if err := os.WriteFile(filepath.Join(cm.cacheDir(), f.Path), []byte("cached:"+f.Name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Write manifest with the given age.
	rec := `{"digest":"` + digest + `","cached_at":"` + time.Now().Add(-age).UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(cm.cacheDir(), "manifest.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- Prefetch tests ---

func TestPrefetchFreshCacheSkipsNetwork(t *testing.T) {
	networkCalled := false
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) {
			networkCalled = true
			return "sha256:new", nil
		},
		func(_ context.Context, _ Config, _ *cacheManager) error {
			networkCalled = true
			return nil
		},
	)
	seedCache(t, c.cm, "sha256:current", 0) // zero age = fresh

	if err := c.Prefetch(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if networkCalled {
		t.Fatal("expected no network call with fresh cache")
	}
}

func TestPrefetchStaleSameDigestRefreshesTimestamp(t *testing.T) {
	const digest = "sha256:unchanged"
	downloadCalled := false
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return digest, nil },
		func(_ context.Context, _ Config, _ *cacheManager) error {
			downloadCalled = true
			return nil
		},
	)
	seedCache(t, c.cm, digest, 2*time.Hour) // stale (MaxAge=1h)

	if err := c.Prefetch(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if downloadCalled {
		t.Fatal("expected no re-download when digest is unchanged")
	}
	// Timestamp should now be fresh.
	if c.cm.isStale(time.Hour) {
		t.Fatal("expected cache to be fresh after timestamp refresh")
	}
}

func TestPrefetchStaleDifferentDigestRedownloads(t *testing.T) {
	downloadCalled := false
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "sha256:new", nil },
		func(_ context.Context, _ Config, cm *cacheManager) error {
			downloadCalled = true
			return cm.writeManifest("sha256:new")
		},
	)
	seedCache(t, c.cm, "sha256:old", 2*time.Hour)

	if err := c.Prefetch(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !downloadCalled {
		t.Fatal("expected download when digest changed")
	}
}

func TestPrefetchNetworkFailureWithCacheReturnsStaleCacheUsed(t *testing.T) {
	networkErr := errors.New("connection refused")
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "", networkErr },
		func(_ context.Context, _ Config, _ *cacheManager) error { return nil },
	)
	seedCache(t, c.cm, "sha256:cached", 2*time.Hour)

	err := c.Prefetch(context.Background())
	if !errors.Is(err, ErrStaleCacheUsed) {
		t.Fatalf("got %v, want ErrStaleCacheUsed", err)
	}
}

func TestPrefetchNetworkFailureNoCacheReturnsError(t *testing.T) {
	networkErr := errors.New("connection refused")
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "", networkErr },
		func(_ context.Context, _ Config, _ *cacheManager) error { return nil },
	)
	// No cache seeded.

	err := c.Prefetch(context.Background())
	if err == nil {
		t.Fatal("expected error with no cache and network failure")
	}
	if !errors.Is(err, ErrNoCache) {
		t.Fatalf("got %v, want error wrapping ErrNoCache", err)
	}
}

// --- Get tests ---

func TestGetOverrideOpensLocalFile(t *testing.T) {
	localFile := filepath.Join(t.TempDir(), "local.gql")
	if err := os.WriteFile(localFile, []byte("local content"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "", errors.New("should not be called") },
		func(_ context.Context, _ Config, _ *cacheManager) error { return errors.New("should not be called") },
	)
	c.cfg.Overrides = map[string]string{"schema": localFile}

	rc, err := c.Get(context.Background(), "schema")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "local content" {
		t.Fatalf("got %q, want %q", got, "local content")
	}
}

func TestGetNotFoundReturnsError(t *testing.T) {
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "sha256:x", nil },
		func(_ context.Context, _ Config, cm *cacheManager) error { return cm.writeManifest("sha256:x") },
	)

	_, err := c.Get(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestGetFreshCacheStreamsFile(t *testing.T) {
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "", errors.New("should not be called") },
		func(_ context.Context, _ Config, _ *cacheManager) error { return errors.New("should not be called") },
	)
	seedCache(t, c.cm, "sha256:current", 0) // fresh

	rc, err := c.Get(context.Background(), "schema")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cached:schema" {
		t.Fatalf("got %q, want %q", got, "cached:schema")
	}
}

func TestGetStaleCacheNetworkFailureFallsBack(t *testing.T) {
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "", errors.New("network down") },
		func(_ context.Context, _ Config, _ *cacheManager) error { return nil },
	)
	seedCache(t, c.cm, "sha256:old", 2*time.Hour) // stale

	// Should still return the cached file via the ErrStaleCacheUsed path.
	rc, err := c.Get(context.Background(), "config")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cached:config" {
		t.Fatalf("got %q, want %q", got, "cached:config")
	}
}

// --- Config / New tests ---

func TestNewRejectsEmptyRegistry(t *testing.T) {
	_, err := New(Config{
		Repository: "org/repo",
		Tag:        "latest",
		Files:      testFiles,
	})
	if err == nil {
		t.Fatal("expected error for empty Registry")
	}
}

func TestNewRejectsNoFiles(t *testing.T) {
	_, err := New(Config{
		Registry:   "r.example.com",
		Repository: "org/repo",
		Tag:        "latest",
	})
	if err == nil {
		t.Fatal("expected error for empty Files")
	}
}

func TestNewAppliesDefaultMaxAge(t *testing.T) {
	c, err := New(Config{
		Registry:   "r.example.com",
		Repository: "org/repo",
		Tag:        "latest",
		Files:      testFiles,
		CacheDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.MaxAge != defaultMaxAge {
		t.Fatalf("MaxAge = %v, want %v", c.cfg.MaxAge, defaultMaxAge)
	}
}

func TestFromEnvMaxAgeParsing(t *testing.T) {
	t.Setenv(EnvRegistry, "r.example.com")
	t.Setenv(EnvRepository, "org/repo")
	t.Setenv(EnvTag, "latest")
	t.Setenv(EnvMaxAge, "48h")
	t.Setenv(EnvCacheDir, t.TempDir())

	c, err := FromEnv(testFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.MaxAge != 48*time.Hour {
		t.Fatalf("MaxAge = %v, want 48h", c.cfg.MaxAge)
	}
}

func TestFromEnvInvalidMaxAge(t *testing.T) {
	t.Setenv(EnvRegistry, "r.example.com")
	t.Setenv(EnvRepository, "org/repo")
	t.Setenv(EnvTag, "latest")
	t.Setenv(EnvMaxAge, "notaduration")

	_, err := FromEnv(testFiles)
	if err == nil {
		t.Fatal("expected error for invalid OCI_MAX_AGE")
	}
}
