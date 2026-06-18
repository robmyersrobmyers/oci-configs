package ociconfigs

import (
	"context"
	"crypto"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	sgroot "github.com/sigstore/sigstore-go/pkg/root"
	sgverify "github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
)

// SigstoreVerifier verifies OCI artifact signatures using the OCI 1.1 Referrers API.
//
// Key-based (recommended for airgapped / private registries):
//
//	ociconfigs.WithVerifier(&ociconfigs.SigstoreVerifier{
//	    KeyPath: "/etc/keys/cosign.pub",
//	})
//
// Key-based verification contacts only the OCI registry — no Rekor, no Fulcio,
// no TUF network access required.
//
// Keyless (GitHub Actions, Google Workload Identity, etc.):
//
//	ociconfigs.WithVerifier(&ociconfigs.SigstoreVerifier{
//	    CertIdentity:   "https://github.com/myorg/myrepo/.github/workflows/release.yml@refs/heads/main",
//	    CertOIDCIssuer: "https://token.actions.githubusercontent.com",
//	    RequireRekor:   true,
//	})
//
// Keyless verification contacts TUF mirrors to fetch the Sigstore trusted root.
// With RequireRekor true it also contacts Rekor for a durable signing timestamp.
// With RequireRekor false (default), the Fulcio certificate is checked against
// the current system clock — only suitable for private Fulcio deployments with
// long-lived certificates.
//
// Registry credentials for fetching signature bundles default to the Docker
// config file (~/.docker/config.json). Set RegistryUsername / RegistryPassword
// or RegistryToken to override.
type SigstoreVerifier struct {
	// KeyPath is the path to a PEM-encoded ECDSA or Ed25519 public key.
	// Mutually exclusive with CertIdentity / CertOIDCIssuer.
	KeyPath string

	// CertIdentity is the expected SAN in the Fulcio signing certificate.
	// Must be paired with CertOIDCIssuer.
	CertIdentity string

	// CertOIDCIssuer is the expected OIDC issuer URL for keyless verification.
	CertOIDCIssuer string

	// RequireRekor requires a Rekor transparency-log inclusion proof.
	// Default false.
	//
	//  - Key-based: Rekor is never contacted regardless of this setting.
	//  - Keyless + false: the Fulcio certificate validity is checked against
	//    the current system clock. Only practical for private Fulcio deployments
	//    with long-lived certificates; the public Sigstore instance issues
	//    ~10-minute certificates.
	//  - Keyless + true: a Rekor inclusion proof is required, providing a
	//    durable timestamp independent of certificate expiry. Recommended when
	// Most configuration files are not going to a Rekor transparency-log.
	RequireRekor bool

	// RegistryUsername / RegistryPassword / RegistryToken are credentials for
	// fetching signature bundles from the OCI registry.
	// Defaults to the Docker config file (~/.docker/config.json).
	RegistryUsername string
	RegistryPassword string
	RegistryToken    string
}

// SigstoreVerifierFromEnv creates a SigstoreVerifier populated from environment variables.
// At least one of OCI_VERIFY_KEY (key-based) or both OCI_VERIFY_CERT_IDENTITY and
// OCI_VERIFY_CERT_ISSUER (keyless) must be set for verification to succeed.
// Registry credentials fall back to the same OCI_USERNAME / OCI_PASSWORD / OCI_TOKEN
// variables used by the rest of the library.
func SigstoreVerifierFromEnv() *SigstoreVerifier {
	v := &SigstoreVerifier{
		KeyPath:          os.Getenv(EnvVerifySigStoreKeyPath),
		CertIdentity:     os.Getenv(EnvVerifySigStoreCertIdentity),
		CertOIDCIssuer:   os.Getenv(EnvVerifySigStoreCertIssuer),
		RegistryUsername: os.Getenv(EnvUsername),
		RegistryPassword: os.Getenv(EnvPassword),
		RegistryToken:    os.Getenv(EnvToken),
	}

	v.RequireRekor, _ = strconv.ParseBool(os.Getenv(EnvVerifySigStoreRequireRekor))

	return v
}

// Verify fetches the Sigstore bundle attached to the artifact via the OCI Referrers API
// and verifies the signature.
func (v *SigstoreVerifier) Verify(ctx context.Context, registry, repository, digest string) error {
	bundleData, err := v.fetchBundleFromOCI(ctx, registry, repository, digest)
	if err != nil {
		return err
	}

	b := &sgbundle.Bundle{}
	if err := b.UnmarshalJSON(bundleData); err != nil {
		return fmt.Errorf("parsing sigstore bundle: %w", err)
	}

	verifier, err := v.buildVerifier()
	if err != nil {
		return err
	}

	policy, err := v.buildPolicy(digest)
	if err != nil {
		return err
	}

	if _, err := verifier.Verify(b, policy); err != nil {
		return fmt.Errorf("signature verification failed for %s/%s@%s: %w", registry, repository, digest, err)
	}

	return nil
}

// fetchBundleFromOCI lists the OCI Referrers of the signed artifact and
// returns the raw JSON of the first Sigstore bundle found.
func (v *SigstoreVerifier) fetchBundleFromOCI(
	ctx context.Context, registry, repository, digest string,
) ([]byte, error) {
	cfg := Config{
		Registry:   registry,
		Repository: repository,
		username:   v.RegistryUsername,
		password:   v.RegistryPassword,
		token:      v.RegistryToken,
	}

	repo, err := newRepository(cfg)
	if err != nil {
		return nil, fmt.Errorf("building registry client: %w", err)
	}

	// Resolve the subject descriptor (digest is already known; size and
	// mediaType are not required for the Referrers API endpoint).
	goDigest, err := parseDigest(digest)
	if err != nil {
		return nil, err
	}

	subjectDesc := ocispec.Descriptor{Digest: goDigest}

	var bundleDesc *ocispec.Descriptor

	if err := repo.Referrers(ctx, subjectDesc, "", func(refs []ocispec.Descriptor) error {
		for i := range refs {
			if bundleDesc == nil && isSigstoreBundle(refs[i].ArtifactType) {
				ref := refs[i]
				bundleDesc = &ref
			}
		}

		return nil
	}); err != nil {
		return nil, fmt.Errorf("listing referrers for %s/%s@%s: %w", registry, repository, digest, err)
	}

	if bundleDesc == nil {
		return nil, fmt.Errorf(
			"%w for %s/%s@%s: SigstoreVerifier could not find signed artifact",
			ErrNoBundleFound, registry, repository, digest,
		)
	}

	return v.fetchBundleBlob(ctx, repo, *bundleDesc)
}

// referrerFetcher is the subset of remote.Repository used by fetchBundleBlob,
// extracted so tests can mock it.
type referrerFetcher interface {
	Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error)
}

// fetchBundleBlob fetches the OCI manifest for a referrer and extracts the
// Sigstore bundle JSON from its first layer blob.
func (v *SigstoreVerifier) fetchBundleBlob(
	ctx context.Context, repo referrerFetcher, manifestDesc ocispec.Descriptor,
) ([]byte, error) {
	// Fetch the referrer manifest.
	rc, err := repo.Fetch(ctx, manifestDesc)
	if err != nil {
		return nil, fmt.Errorf("fetching referrer manifest: %w", err)
	}

	manifestBytes, err := io.ReadAll(rc)
	_ = rc.Close()

	if err != nil {
		return nil, fmt.Errorf("reading referrer manifest: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing referrer manifest: %w", err)
	}

	if len(manifest.Layers) == 0 {
		return nil, ErrNoLayers
	}

	// Fetch the bundle blob from the first layer.
	blobRC, err := repo.Fetch(ctx, manifest.Layers[0])
	if err != nil {
		return nil, fmt.Errorf("fetching bundle blob: %w", err)
	}

	data, err := io.ReadAll(blobRC)
	_ = blobRC.Close()

	if err != nil {
		return nil, fmt.Errorf("reading bundle blob: %w", err)
	}

	return data, nil
}

// buildVerifier constructs the sigstore-go Verifier appropriate for the
// configured authentication mode (key-based or keyless).
func (v *SigstoreVerifier) buildVerifier() (*sgverify.Verifier, error) {
	if v.KeyPath != "" {
		return v.buildKeyVerifier()
	}

	return v.buildKeylessVerifier()
}

// buildKeyVerifier creates a Verifier that checks signatures against a PEM
// public key. Rekor is never consulted — safe for airgapped deployments.
// Creates a key with zero validity period and no observer timestamps to ensure
// key is considered valid at all times.
func (v *SigstoreVerifier) buildKeyVerifier() (*sgverify.Verifier, error) {
	keyPEM, err := os.ReadFile(v.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading public key %q: %w", v.KeyPath, err)
	}

	pubKey, err := cryptoutils.UnmarshalPEMToPublicKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing public key from %q: %w", v.KeyPath, err)
	}

	sigVerifier, err := signature.LoadVerifier(pubKey, crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("loading signature verifier: %w", err)
	}

	nonExpiringKey := sgroot.NewExpiringKey(sigVerifier, time.Time{}, time.Time{})
	trustedMaterial := sgroot.NewTrustedPublicKeyMaterialFromMapping(
		map[string]*sgroot.ExpiringKey{"": nonExpiringKey},
	)

	vr, err := sgverify.NewVerifier(trustedMaterial, sgverify.WithNoObserverTimestamps())
	if err != nil {
		return nil, fmt.Errorf("creating key verifier: %w", err)
	}

	return vr, nil
}

// buildKeylessVerifier creates a Verifier that checks signatures made with
// short-lived Fulcio certificates (OIDC-based).
// Requires outbound access to TUF mirrors to fetch the Sigstore trusted root.
func (v *SigstoreVerifier) buildKeylessVerifier() (*sgverify.Verifier, error) {
	trustedRoot, err := sgroot.FetchTrustedRoot()
	if err != nil {
		return nil, fmt.Errorf("fetching Sigstore trusted root from TUF: %w", err)
	}

	var opts []sgverify.VerifierOption
	if v.RequireRekor {
		opts = append(opts, sgverify.WithTransparencyLog(1))
	} else {
		// Without Rekor, use the current system clock to check whether the
		// Fulcio certificate was valid at verification time.
		// For production use with public Sigstore, set RequireRekor: true.
		opts = append(opts, sgverify.WithCurrentTime())
	}

	vr, err := sgverify.NewVerifier(trustedRoot, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating keyless verifier: %w", err)
	}

	return vr, nil
}

// buildPolicy constructs the sigstore-go PolicyBuilder for the given artifact
// digest and the configured identity mode.
func (v *SigstoreVerifier) buildPolicy(digest string) (sgverify.PolicyBuilder, error) {
	alg, hexStr, ok := strings.Cut(digest, ":")
	if !ok {
		return sgverify.PolicyBuilder{}, fmt.Errorf("%w: got %q", ErrMalformedDigest, digest)
	}

	digestBytes, err := hex.DecodeString(hexStr)
	if err != nil {
		return sgverify.PolicyBuilder{}, fmt.Errorf("decoding digest hex in %q: %w", digest, err)
	}

	artifactOpt := sgverify.WithArtifactDigest(alg, digestBytes)

	if v.KeyPath != "" {
		return sgverify.NewPolicy(artifactOpt, sgverify.WithKey()), nil
	}

	identity, err := sgverify.NewShortCertificateIdentity(
		v.CertOIDCIssuer, "", v.CertIdentity, "",
	)
	if err != nil {
		return sgverify.PolicyBuilder{}, fmt.Errorf("building certificate identity: %w", err)
	}

	return sgverify.NewPolicy(artifactOpt, sgverify.WithCertificateIdentity(identity)), nil
}

// isSigstoreBundle reports whether an OCI referrer artifactType identifies a Sigstore bundle.
func isSigstoreBundle(artifactType string) bool {
	return strings.HasPrefix(artifactType, "application/vnd.dev.sigstore.bundle")
}

// parseDigest parses a "alg:hex" digest string into an opencontainers Digest.
func parseDigest(digest string) (godigest.Digest, error) {
	d := godigest.Digest(digest)

	err := d.Validate()
	if err != nil {
		return "", fmt.Errorf("invalid digest %q: %w", digest, err)
	}

	return d, nil
}
