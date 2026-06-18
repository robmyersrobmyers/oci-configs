package ociconfigs

import (
	"context"
	"fmt"
	"io"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// pull downloads all configured artifact files to the cache directory and
// writes the manifest record with the resolved digest on success.
func pull(ctx context.Context, cfg Config, cm *cacheManager) error {
	repo, err := newRepository(cfg)
	if err != nil {
		return err
	}

	store, err := file.New(cm.cacheDir())
	if err != nil {
		return fmt.Errorf("creating file store: %w", err)
	}

	defer func() { _ = store.Close() }()

	target := &progressTarget{GraphTarget: store, onProgress: cfg.OnProgress}

	desc, err := oras.Copy(ctx, repo, cfg.Tag, target, cfg.Tag, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("pulling artifact %s/%s:%s: %w", cfg.Registry, cfg.Repository, cfg.Tag, err)
	}

	digest := desc.Digest.String()
	verifiedDigest := ""

	if cfg.Verifier != nil {
		if err := runVerifier(ctx, cfg, digest); err != nil {
			return err
		}

		verifiedDigest = digest
	}

	return cm.writeManifest(digest, verifiedDigest)
}

// fetchRemoteDigest resolves the manifest digest for cfg.Tag without
// downloading any blob content.
func fetchRemoteDigest(ctx context.Context, cfg Config) (string, error) {
	repo, err := newRepository(cfg)
	if err != nil {
		return "", err
	}

	desc, err := repo.Resolve(ctx, cfg.Tag)
	if err != nil {
		return "", fmt.Errorf("resolving %s/%s:%s: %w", cfg.Registry, cfg.Repository, cfg.Tag, err)
	}

	return desc.Digest.String(), nil
}

// newRepository builds an authenticated remote.Repository for cfg.
func newRepository(cfg Config) (*remote.Repository, error) {
	ref := cfg.Registry + "/" + cfg.Repository

	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid repository reference %q: %w", ref, err)
	}

	repo.Client = &auth.Client{
		Credential: buildCredentialFunc(cfg),
		Cache:      auth.NewCache(),
	}

	return repo, nil
}

// progressTarget wraps an oras.GraphTarget to emit ProgressEvents as file
// blobs are received via Push.
type progressTarget struct {
	oras.GraphTarget

	onProgress ProgressFunc
}

func (t *progressTarget) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	if t.onProgress == nil || expected.Annotations == nil {
		return t.GraphTarget.Push(ctx, expected, content) //nolint:wrapcheck // delegating wrapper
	}

	name := expected.Annotations[ocispec.AnnotationTitle]
	if name == "" {
		return t.GraphTarget.Push(ctx, expected, content) //nolint:wrapcheck // delegating wrapper
	}

	pr := &progressReader{
		Reader:     content,
		name:       name,
		total:      expected.Size,
		onProgress: t.onProgress,
	}

	return t.GraphTarget.Push(ctx, expected, pr) //nolint:wrapcheck // delegating wrapper
}

// progressReader wraps an io.Reader and emits a ProgressEvent after each Read.
type progressReader struct {
	io.Reader

	name       string
	total      int64
	completed  int64
	onProgress ProgressFunc
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.completed += int64(n)
	done := err == io.EOF
	r.onProgress(ProgressEvent{
		Name:      r.name,
		Total:     r.total,
		Completed: r.completed,
		Done:      done,
	})

	return n, err //nolint:wrapcheck
}

// runVerifier invokes cfg.Verifier and emits structured log lines around the call.
// Callers must ensure cfg.Verifier is non-nil before calling.
func runVerifier(ctx context.Context, cfg Config, digest string) error {
	if cfg.Logger != nil {
		cfg.Logger.Debug("verifying artifact signature",
			"registry", cfg.Registry,
			"repository", cfg.Repository,
			"digest", digest,
		)
	}

	if err := cfg.Verifier.Verify(ctx, cfg.Registry, cfg.Repository, digest); err != nil {
		if cfg.Logger != nil {
			cfg.Logger.Warn("artifact signature verification failed",
				"registry", cfg.Registry,
				"repository", cfg.Repository,
				"digest", digest,
				"err", err,
			)
		}

		return fmt.Errorf("%w: %w", ErrVerificationFailed, err)
	}

	if cfg.Logger != nil {
		cfg.Logger.Info("artifact signature verified",
			"registry", cfg.Registry,
			"repository", cfg.Repository,
			"digest", digest,
		)
	}

	return nil
}
