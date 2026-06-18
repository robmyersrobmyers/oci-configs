package ociconfigs

import "errors"

var (
	// ErrNoCache is returned when a file is requested but no local cache exists
	// and the registry is unreachable.
	ErrNoCache = errors.New("no cached data available")

	// ErrRegistryRequired is returned when the corresponding Config field is missing.
	ErrRegistryRequired = errors.New("registry is required")
	// ErrRepositoryRequired is returned when the corresponding Config field is missing.
	ErrRepositoryRequired = errors.New("repository is required")
	// ErrTagRequired is returned when the corresponding Config field is missing.
	ErrTagRequired = errors.New("tag is required")
	// ErrFilesRequired is returned when the corresponding Config field is missing.
	ErrFilesRequired = errors.New("at least one FileSpec is required")

	// ErrNotFound is returned when a logical name does not match any configured FileSpec.
	ErrNotFound = errors.New("file not found in config")

	// ErrStaleCacheUsed is returned (non-fatally) when the remote digest check
	// failed but valid cached files exist and were used instead.
	ErrStaleCacheUsed = errors.New("remote check failed; using stale cache")

	// ErrVerificationFailed is returned when an ArtifactVerifier rejects the
	// downloaded artifact. The previous cache (if any) is preserved.
	ErrVerificationFailed = errors.New("artifact verification failed")

	// ErrNoBundleFound is returned when no Sigstore bundle referrer is found for the artifact.
	ErrNoBundleFound = errors.New("no sigstore bundle found")
	// ErrNoLayers is returned when a referrer manifest has no layer blobs.
	ErrNoLayers = errors.New("referrer manifest has no layers")
	// ErrMalformedDigest is returned when a digest string is not in "alg:hex" format.
	ErrMalformedDigest = errors.New("malformed digest: expected alg:hex")
)
