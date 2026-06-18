// Package ociconfigs manages application configuration files stored in OCI
// artifact registries, with a persistent local disk cache and optional
// signature verification.
package ociconfigs

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Environment variable names read by FromEnv and applyDefaults.
const (
	defaultMaxAge = 7 * 24 * time.Hour

	EnvRegistry   = "OCI_REGISTRY"
	EnvRepository = "OCI_REPOSITORY"
	EnvTag        = "OCI_TAG"
	EnvUsername   = "OCI_USERNAME"
	EnvPassword   = "OCI_PASSWORD"
	EnvToken      = "OCI_TOKEN"
	EnvCacheDir   = "OCI_CACHE_DIR"
	EnvMaxAge     = "OCI_MAX_AGE"

	// EnvVerifySigStoreKeyPath and the following constants are env vars read by SigstoreVerifierFromEnv.
	EnvVerifySigStoreKeyPath      = "OCI_VERIFY_SIGSTORE_KEY"
	EnvVerifySigStoreCertIdentity = "OCI_VERIFY_SIGSTORE_CERT_IDENTITY"
	EnvVerifySigStoreCertIssuer   = "OCI_VERIFY_SIGSTORE_CERT_ISSUER"
	EnvVerifySigStoreRequireRekor = "OCI_VERIFY_SIGSTORE_REQUIRE_REKOR"
)

// FileSpec maps a logical name to a file stored in an OCI artifact.
type FileSpec struct {
	// Name is the logical name used to retrieve the file, e.g. "schema".
	Name string
	// Path is the file name in the OCI artifact, e.g. "schema.gql".
	Path string
	// MediaType is the OCI media type, e.g. "application/graphql+text".
	MediaType string
}

// Config holds the configuration for a Client.
type Config struct {
	// Registry is the hostname of the OCI registry, e.g. "registry.example.com".
	Registry string
	// Repository is the image repository path, e.g. "myorg/configs".
	Repository string
	// Tag is the image tag or digest to pull, e.g. "latest".
	Tag string
	// Files lists the artifact files to manage.
	Files []FileSpec
	// CacheDir is the directory for the local file cache.
	// Defaults to os.UserCacheDir()/oci-configs.
	CacheDir string
	// MaxAge is how long cached files are considered fresh before re-checking.
	// Defaults to 7 days.
	MaxAge time.Duration
	// Overrides maps a logical file name to a local file path.
	// When set, Get returns the local file instead of the cached artifact.
	Overrides map[string]string
	// OnProgress is called with download progress events. May be nil.
	OnProgress ProgressFunc
	// Logger receives structured log output. May be nil.
	Logger *slog.Logger

	// Verifier is called after every successful download to validate artifact
	// signatures. If nil, no signature verification is performed.
	Verifier ArtifactVerifier

	// auth fields set via functional options; not exported to avoid leaking secrets.
	username string
	password string
	token    string
}

// Option is a functional option for configuring a Client.
type Option func(*Config)

// WithCacheDir sets a custom cache directory.
func WithCacheDir(dir string) Option {
	return func(c *Config) { c.CacheDir = dir }
}

// WithMaxAge sets how long cached files are considered fresh.
func WithMaxAge(d time.Duration) Option {
	return func(c *Config) { c.MaxAge = d }
}

// WithOverride adds a local file path override for the named file.
// When set, Get(ctx, name) opens the local file instead of the cache.
func WithOverride(name, path string) Option {
	return func(c *Config) {
		if c.Overrides == nil {
			c.Overrides = make(map[string]string)
		}

		c.Overrides[name] = path
	}
}

// WithProgress sets a callback for download progress events.
func WithProgress(fn ProgressFunc) Option {
	return func(c *Config) { c.OnProgress = fn }
}

// WithLogger sets a structured logger for diagnostic output.
func WithLogger(l *slog.Logger) Option {
	return func(c *Config) { c.Logger = l }
}

// WithCredentials sets explicit username and password credentials.
// These take priority over environment variables and the docker config file.
func WithCredentials(username, password string) Option {
	return func(c *Config) {
		c.username = username
		c.password = password
	}
}

// WithToken sets an explicit bearer token credential.
// Takes priority over environment variables and the docker config file.
func WithToken(token string) Option {
	return func(c *Config) { c.token = token }
}

// applyDefaults fills zero values with sensible defaults.
func (c *Config) applyDefaults() error {
	if c.MaxAge == 0 {
		c.MaxAge = defaultMaxAge
	}

	if c.CacheDir == "" {
		if env := os.Getenv(EnvCacheDir); env != "" {
			c.CacheDir = env
		} else {
			base, err := os.UserCacheDir()
			if err != nil {
				return fmt.Errorf("resolving cache directory: %w", err)
			}

			c.CacheDir = filepath.Join(base, "oci-configs")
		}
	}

	return nil
}

// validate checks that required fields are present.
func (c *Config) validate() error {
	if c.Registry == "" {
		return ErrRegistryRequired
	}

	if c.Repository == "" {
		return ErrRepositoryRequired
	}

	if c.Tag == "" {
		return ErrTagRequired
	}

	if len(c.Files) == 0 {
		return ErrFilesRequired
	}

	return nil
}

// FromEnv creates a Client by reading OCI_* environment variables.
// Any opts are applied after env vars and may override them.
func FromEnv(files []FileSpec, opts ...Option) (*Client, error) {
	cfg := Config{
		Registry:   os.Getenv(EnvRegistry),
		Repository: os.Getenv(EnvRepository),
		Tag:        os.Getenv(EnvTag),
		Files:      files,
	}

	if raw := os.Getenv(EnvMaxAge); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("parsing %s %q: %w", EnvMaxAge, raw, err)
		}

		cfg.MaxAge = d
	}

	// Wire sigstore verification when at least one meaningful env var is set.
	// OCI_VERIFY_SIGSTORE_REQUIRE_REKOR alone is not sufficient — it is a
	// modifier, not a selector.
	if os.Getenv(EnvVerifySigStoreKeyPath) != "" ||
		os.Getenv(EnvVerifySigStoreCertIdentity) != "" ||
		os.Getenv(EnvVerifySigStoreCertIssuer) != "" {
		cfg.Verifier = SigstoreVerifierFromEnv()
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	return New(cfg)
}
