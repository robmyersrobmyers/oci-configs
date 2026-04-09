package ociconfigs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// manifestRecord is persisted to disk to track cache freshness and digest.
type manifestRecord struct {
	Digest   string    `json:"digest"`
	CachedAt time.Time `json:"cached_at"`
}

// cacheManager manages a local on-disk cache for a specific registry/repository/tag.
type cacheManager struct {
	dir string
}

// newCacheManager returns a cacheManager whose directory is derived from the
// registry, repository, and tag so that different images use separate caches.
func newCacheManager(baseDir, registry, repository, tag string) *cacheManager {
	key := strings.NewReplacer("/", "_", ":", "_").Replace(
		registry + "/" + repository + ":" + tag,
	)
	return &cacheManager{dir: filepath.Join(baseDir, key)}
}

// cacheDir returns the root directory for this cache entry.
func (c *cacheManager) cacheDir() string { return c.dir }

// manifestPath returns the full path of the manifest record file.
func (c *cacheManager) manifestPath() string {
	return filepath.Join(c.dir, "manifest.json")
}

// filePath returns the full cache path for an artifact file given its Path field.
func (c *cacheManager) filePath(path string) string {
	return filepath.Join(c.dir, filepath.Base(path))
}

// readManifest reads and decodes the persisted manifest record.
func (c *cacheManager) readManifest() (*manifestRecord, error) {
	data, err := os.ReadFile(c.manifestPath())
	if err != nil {
		return nil, err
	}
	var rec manifestRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decoding manifest cache: %w", err)
	}
	return &rec, nil
}

// writeManifest persists a manifest record with the given digest and the
// current UTC time as cached_at.
func (c *cacheManager) writeManifest(digest string) error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}
	rec := manifestRecord{Digest: digest, CachedAt: time.Now().UTC()}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding manifest cache: %w", err)
	}
	return os.WriteFile(c.manifestPath(), data, 0o600)
}

// isStale returns true if no manifest exists or the cached_at timestamp is
// older than maxAge.
func (c *cacheManager) isStale(maxAge time.Duration) bool {
	rec, err := c.readManifest()
	if err != nil {
		return true
	}
	return time.Since(rec.CachedAt) > maxAge
}

// digest returns the last-known digest from the manifest, or "" if unavailable.
func (c *cacheManager) digest() string {
	rec, err := c.readManifest()
	if err != nil {
		return ""
	}
	return rec.Digest
}

// hasFile reports whether the cached file for the given artifact path exists.
func (c *cacheManager) hasFile(path string) bool {
	_, err := os.Stat(c.filePath(path))
	return err == nil
}

// allFilesCached reports whether every FileSpec in files has a cached copy.
func (c *cacheManager) allFilesCached(files []FileSpec) bool {
	for _, f := range files {
		if !c.hasFile(f.Path) {
			return false
		}
	}
	return true
}

// openFile returns a ReadCloser for the cached file at the given artifact path.
func (c *cacheManager) openFile(path string) (io.ReadCloser, error) {
	return os.Open(c.filePath(path))
}
