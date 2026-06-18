package ociconfigs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Client manages configuration files sourced from an OCI artifact registry,
// with a persistent local disk cache.
type Client struct {
	cfg Config
	cm  *cacheManager

	// fetchDigest and download are injectable for testing.
	fetchDigest func(context.Context, Config) (string, error)
	download    func(context.Context, Config, *cacheManager) error
}

// New creates a new Client from cfg, applying any additional opts.
// New validates the final configuration and applies defaults but makes no
// network calls.
func New(cfg Config, opts ...Option) (*Client, error) {
	for _, opt := range opts {
		opt(&cfg)
	}

	err := cfg.applyDefaults()
	if err != nil {
		return nil, err
	}

	err = cfg.validate()
	if err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	cm := newCacheManager(cfg.CacheDir, cfg.Registry, cfg.Repository, cfg.Tag)

	return &Client{
		cfg:         cfg,
		cm:          cm,
		fetchDigest: fetchRemoteDigest,
		download:    pull,
	}, nil
}

// Prefetch ensures all configured files are locally cached and up to date.
//
//   - If the cache is fresh (within MaxAge) and all files are present, returns immediately.
//   - If the cache is stale, the remote manifest digest is fetched.
//     If the digest is unchanged, only the cached_at timestamp is refreshed.
//     If the digest changed, all files are re-downloaded.
//   - If the network is unavailable and cached files exist, a warning is logged
//     and ErrStaleCacheUsed is returned (non-fatal).
//   - If the network is unavailable and no cache exists, an error is returned.
func (c *Client) Prefetch(ctx context.Context) error {
	if c.freshCacheValid() {
		c.logDebug("cache is fresh, skipping network check")

		return nil
	}

	remoteDigest, err := c.fetchDigest(ctx, c.cfg)
	if err != nil {
		return c.handleNetworkError(err)
	}

	if remoteDigest == c.cm.digest() && c.cm.allFilesCached(c.cfg.Files) {
		return c.handleDigestUnchanged(ctx, remoteDigest)
	}

	c.logInfo("downloading artifact",
		"registry", c.cfg.Registry,
		"repository", c.cfg.Repository,
		"tag", c.cfg.Tag,
	)

	return c.download(ctx, c.cfg, c.cm)
}

// Get returns an io.ReadCloser for the named file. The caller must call
// Close() on the returned ReadCloser when done.
//
// Priority:
//  1. If an override path is configured for name, that local file is opened.
//  2. If the cache is present (possibly stale), the cached file is streamed.
//     A stale cache triggers a Prefetch first; if Prefetch fails non-fatally
//     (ErrStaleCacheUsed), the existing cached file is used.
//  3. If no cache exists, Prefetch is called; errors are returned directly.
func (c *Client) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if path, ok := c.cfg.Overrides[name]; ok {
		f, err := os.Open(path) //nolint:gosec // override paths are user-supplied by design
		if err != nil {
			return nil, fmt.Errorf("opening override for %q: %w", name, err)
		}

		return f, nil
	}

	spec, err := c.fileSpec(name)
	if err != nil {
		return nil, err
	}

	needsDownload := !c.cm.hasFile(spec.Path)
	if needsDownload || c.cm.isStale(c.cfg.MaxAge) {
		prefetchErr := c.Prefetch(ctx)
		if prefetchErr != nil && !errors.Is(prefetchErr, ErrStaleCacheUsed) {
			return nil, prefetchErr
		}
	}

	rc, err := c.cm.openFile(spec.Path)
	if err != nil {
		return nil, fmt.Errorf("opening cached file for %q: %w", name, err)
	}

	return rc, nil
}

// CacheEntry describes a single cached file.
type CacheEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// CacheStatus holds the result of CacheInfo.
type CacheStatus struct {
	CacheDir string       `json:"cache_dir"`
	Digest   string       `json:"digest"`
	CachedAt time.Time    `json:"cached_at"`
	Fresh    bool         `json:"fresh"`
	Files    []CacheEntry `json:"files"`
}

// CacheInfo returns the current state of the local cache without making any
// network calls.
func (c *Client) CacheInfo() (CacheStatus, error) {
	status := CacheStatus{CacheDir: c.cm.cacheDir()}

	rec, err := c.cm.readManifest()
	if err == nil {
		status.Digest = rec.Digest
		status.CachedAt = rec.CachedAt
		status.Fresh = !c.cm.isStale(c.cfg.MaxAge)
	}

	for _, f := range c.cfg.Files {
		entry := CacheEntry{Name: f.Name, Path: f.Path}

		info, statErr := os.Stat(c.cm.filePath(f.Path))
		if statErr == nil {
			entry.Size = info.Size()
		}

		status.Files = append(status.Files, entry)
	}

	return status, nil
}

// Close releases any resources held by the Client. Currently a no-op but
// included for future compatibility.
func (c *Client) Close() error { return nil }

// freshCacheValid reports whether the cache is current enough to skip the
// network entirely. Returns false when stale, files are missing, or a verifier
// is configured but the current artifact has not yet been verified.
func (c *Client) freshCacheValid() bool {
	if c.cm.isStale(c.cfg.MaxAge) || !c.cm.allFilesCached(c.cfg.Files) {
		return false
	}

	return c.cfg.Verifier == nil || c.cm.isVerified()
}

// handleNetworkError converts a digest-fetch failure into the appropriate
// return value: ErrStaleCacheUsed when cached files exist, or a wrapped
// ErrNoCache when there is nothing to fall back to.
func (c *Client) handleNetworkError(err error) error {
	if c.cm.allFilesCached(c.cfg.Files) {
		c.logWarn("remote check failed; using stale cache", "err", err)

		return ErrStaleCacheUsed
	}

	return fmt.Errorf("%w: %w", ErrNoCache, err)
}

// handleDigestUnchanged handles the path where the remote digest matches the
// cached digest. It runs verification when a verifier is configured and the
// current digest has not been verified before, then refreshes the timestamp.
func (c *Client) handleDigestUnchanged(ctx context.Context, remoteDigest string) error {
	if c.cfg.Verifier != nil && c.cm.verifiedDigest() != remoteDigest {
		c.logDebug("verifying artifact signature",
			"registry", c.cfg.Registry,
			"repository", c.cfg.Repository,
			"digest", remoteDigest,
		)

		if err := c.cfg.Verifier.Verify(ctx, c.cfg.Registry, c.cfg.Repository, remoteDigest); err != nil {
			c.logWarn("artifact signature verification failed",
				"registry", c.cfg.Registry,
				"repository", c.cfg.Repository,
				"digest", remoteDigest,
				"err", err,
			)

			return fmt.Errorf("%w: %w", ErrVerificationFailed, err)
		}

		c.logInfo("artifact signature verified",
			"registry", c.cfg.Registry,
			"repository", c.cfg.Repository,
			"digest", remoteDigest,
		)

		return c.cm.writeManifest(remoteDigest, remoteDigest)
	}

	c.logDebug("digest unchanged, refreshing cache timestamp")

	return c.cm.writeManifest(remoteDigest, c.cm.verifiedDigest())
}

func (c *Client) fileSpec(name string) (FileSpec, error) {
	for _, f := range c.cfg.Files {
		if f.Name == name {
			return f, nil
		}
	}

	return FileSpec{}, fmt.Errorf("%w: %q", ErrNotFound, name)
}

func (c *Client) logInfo(msg string, args ...any) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Info(msg, args...)
	}
}

func (c *Client) logWarn(msg string, args ...any) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Warn(msg, args...)
	}
}

func (c *Client) logDebug(msg string, args ...any) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Debug(msg, args...)
	}
}

// compile-time interface check.
var _ interface {
	Prefetch(ctx context.Context) error
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	CacheInfo() (CacheStatus, error)
	Close() error
} = (*Client)(nil)
