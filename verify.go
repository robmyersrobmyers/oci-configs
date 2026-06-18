package ociconfigs

import "context"

// ArtifactVerifier is an optional post-download hook for supply-chain security.
// Verify is called after every successful artifact download.
// Return a non-nil error to reject the artifact.
type ArtifactVerifier interface {
	Verify(ctx context.Context, registry, repository, digest string) error
}

// WithVerifier attaches an ArtifactVerifier to the client.
func WithVerifier(v ArtifactVerifier) Option {
	return func(c *Config) { c.Verifier = v }
}
