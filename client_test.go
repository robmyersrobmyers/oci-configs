package ociconfigs

import (
	"context"
	"errors"
	"fmt"
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
func makeClient(
	t *testing.T,
	fetchDigest func(context.Context, Config) (string, error),
	download func(context.Context, Config, *cacheManager) error,
) *Client {
	t.Helper()

	cfg := Config{
		Registry:   "registry.example.com",
		Repository: "myorg/configs",
		Tag:        "latest",
		Files:      testFiles,
		CacheDir:   t.TempDir(),
		MaxAge:     time.Hour,
	}

	err := cfg.applyDefaults()
	if err != nil {
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

	err := os.MkdirAll(cm.cacheDir(), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range testFiles {
		err := os.WriteFile(filepath.Join(cm.cacheDir(), f.Path), []byte("cached:"+f.Name), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Write manifest with the given age.
	rec := `{"digest":"` + digest + `","cached_at":"` + time.Now().Add(-age).UTC().Format(time.RFC3339) + `"}`

	err = os.WriteFile(filepath.Join(cm.cacheDir(), "manifest.json"), []byte(rec), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// --- Prefetch tests ---

func TestPrefetchFreshCacheSkipsNetwork(t *testing.T) {
	t.Parallel()

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

	err := c.Prefetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if networkCalled {
		t.Fatal("expected no network call with fresh cache")
	}
}

func TestPrefetchStaleSameDigestRefreshesTimestamp(t *testing.T) {
	t.Parallel()

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

	err := c.Prefetch(context.Background())
	if err != nil {
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
	t.Parallel()

	downloadCalled := false
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "sha256:new", nil },
		func(_ context.Context, _ Config, cm *cacheManager) error {
			downloadCalled = true

			return cm.writeManifest("sha256:new", "")
		},
	)
	seedCache(t, c.cm, "sha256:old", 2*time.Hour)

	err := c.Prefetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !downloadCalled {
		t.Fatal("expected download when digest changed")
	}
}

func TestPrefetchNetworkFailureWithCacheReturnsStaleCacheUsed(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

func TestGetOverrideOpensLocalFile(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "sha256:x", nil },
		func(_ context.Context, _ Config, cm *cacheManager) error { return cm.writeManifest("sha256:x", "") },
	)

	_, err := c.Get(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestGetFreshCacheStreamsFile(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

func TestNewRejectsEmptyRegistry(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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

// --- Verifier tests ---

// stubVerifier records calls and optionally returns an error.
type stubVerifier struct {
	calls  []string // "registry/repository@digest" per Verify call
	errMsg string   // if non-empty, Verify returns this error
}

func (s *stubVerifier) Verify(_ context.Context, registry, repository, digest string) error {
	s.calls = append(s.calls, registry+"/"+repository+"@"+digest)
	if s.errMsg != "" {
		return errors.New(s.errMsg)
	}

	return nil
}

func TestPrefetchVerifierCalledAfterDownload(t *testing.T) {
	t.Parallel()

	v := &stubVerifier{}
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "sha256:new", nil },
		func(ctx context.Context, cfg Config, cm *cacheManager) error {
			if cfg.Verifier == nil {
				t.Error("verifier not propagated to download func")
			}
			// Simulate what pull() does: verify then write manifest.
			err := cfg.Verifier.Verify(ctx, cfg.Registry, cfg.Repository, "sha256:new")
			if err != nil {
				return fmt.Errorf("%w: %w", ErrVerificationFailed, err)
			}

			return cm.writeManifest("sha256:new", "sha256:new")
		},
	)
	c.cfg.Verifier = v

	err := c.Prefetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(v.calls) != 1 {
		t.Fatalf("expected 1 verifier call, got %d", len(v.calls))
	}
}

func TestPrefetchVerifierFailureReturnsError(t *testing.T) {
	t.Parallel()

	v := &stubVerifier{errMsg: "bad signature"}
	downloadCalled := false
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "sha256:new", nil },
		func(ctx context.Context, cfg Config, cm *cacheManager) error {
			downloadCalled = true

			if cfg.Verifier != nil {
				err := cfg.Verifier.Verify(ctx, cfg.Registry, cfg.Repository, "sha256:new")
				if err != nil {
					return fmt.Errorf("%w: %w", ErrVerificationFailed, err)
				}
			}

			return cm.writeManifest("sha256:new", "sha256:new")
		},
	)
	c.cfg.Verifier = v
	seedCache(t, c.cm, "sha256:old", 2*time.Hour)

	err := c.Prefetch(context.Background())
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("got %v, want ErrVerificationFailed", err)
	}

	if !downloadCalled {
		t.Fatal("expected download to have been attempted")
	}
	// Manifest must still hold the old digest (cache not updated after failed verify).
	if got := c.cm.digest(); got != "sha256:old" {
		t.Fatalf("cache digest = %q, want sha256:old", got)
	}
}

func TestPrefetchVerifierSkippedWhenFreshAndVerified(t *testing.T) {
	t.Parallel()

	v := &stubVerifier{}
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) {
			t.Error("fetchDigest must not be called with fresh verified cache")

			return "", nil
		},
		func(_ context.Context, _ Config, _ *cacheManager) error {
			t.Error("download must not be called with fresh verified cache")

			return nil
		},
	)
	c.cfg.Verifier = v
	// Seed a cache that is fresh and already verified.
	seedCache(t, c.cm, "sha256:current", 0)

	err := c.cm.writeManifest("sha256:current", "sha256:current")
	if err != nil {
		t.Fatal(err)
	}

	err = c.Prefetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(v.calls) != 0 {
		t.Fatalf("expected no verifier calls, got %d", len(v.calls))
	}
}

func TestPrefetchVerifierCalledForUnverifiedCachedDigest(t *testing.T) {
	t.Parallel()

	// Simulates adding WithVerifier after the initial pull: the cache has a valid
	// digest but verified_digest is empty. Prefetch should verify without re-downloading.
	v := &stubVerifier{}
	downloadCalled := false
	c := makeClient(t,
		func(_ context.Context, _ Config) (string, error) { return "sha256:current", nil },
		func(_ context.Context, _ Config, _ *cacheManager) error {
			downloadCalled = true

			return nil
		},
	)
	c.cfg.Verifier = v
	// Seed cache stale so we reach the digest-compare path, unverified.
	seedCache(t, c.cm, "sha256:current", 2*time.Hour)

	err := c.Prefetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if downloadCalled {
		t.Fatal("should not re-download when only verification is missing")
	}

	if len(v.calls) != 1 {
		t.Fatalf("expected 1 verifier call, got %d", len(v.calls))
	}

	if got := c.cm.verifiedDigest(); got != "sha256:current" {
		t.Fatalf("verified_digest = %q, want sha256:current", got)
	}
}
