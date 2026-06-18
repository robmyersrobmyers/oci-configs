package ociconfigs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// writeECDSAPublicKey generates a P-256 key pair, writes the PEM-encoded
// public key to a temp file, and returns the path.
func writeECDSAPublicKey(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	derBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: derBytes})

	path := filepath.Join(t.TempDir(), "cosign.pub")

	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// mockFetcher implements referrerFetcher by returning pre-loaded byte slices.
type mockFetcher struct {
	responses map[godigest.Digest][]byte
}

func (m *mockFetcher) Fetch(_ context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	data, ok := m.responses[desc.Digest]
	if !ok {
		return nil, fmt.Errorf("no mock response for digest %s", desc.Digest)
	}

	return io.NopCloser(strings.NewReader(string(data))), nil
}

func TestParseDigest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		wantAlg string
		wantErr bool
	}{
		{
			name:    "valid sha256",
			input:   "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			wantAlg: "sha256",
		},
		{
			name:    "malformed",
			input:   "notadigest",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d, err := parseDigest(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if d.Algorithm().String() != tc.wantAlg {
				t.Fatalf("algorithm = %q, want %q", d.Algorithm(), tc.wantAlg)
			}
		})
	}
}

func TestIsSigstoreBundle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mt   string
		want bool
	}{
		{"sigstore bundle v0.3", "application/vnd.dev.sigstore.bundle+json;version=0.3", true},
		{"sigstore bundle v0.2", "application/vnd.dev.sigstore.bundle+json;version=0.2", true},
		{"sigstore bundle alt format", "application/vnd.dev.sigstore.bundle.v0.3+json", true},
		{"OCI image manifest", "application/vnd.oci.image.manifest.v1+json", false},
		{"empty", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isSigstoreBundle(tc.mt); got != tc.want {
				t.Errorf("isSigstoreBundle(%q) = %v, want %v", tc.mt, got, tc.want)
			}
		})
	}
}

func TestBuildKeyVerifier(t *testing.T) { //nolint:funlen // table-driven test data
	t.Parallel()

	cases := []struct {
		name       string
		setup      func(t *testing.T) string
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:  "valid ECDSA key",
			setup: writeECDSAPublicKey,
		},
		{
			name: "missing file",
			setup: func(t *testing.T) string {
				t.Helper()

				return "/nonexistent/cosign.pub"
			},
			wantErr:    true,
			wantErrMsg: "reading public key",
		},
		{
			name: "invalid PEM",
			setup: func(t *testing.T) string {
				t.Helper()

				path := filepath.Join(t.TempDir(), "bad.pub")
				if err := os.WriteFile(path, []byte("not a pem key"), 0o600); err != nil {
					t.Fatal(err)
				}

				return path
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v := &SigstoreVerifier{KeyPath: tc.setup(t)}

			verifier, err := v.buildKeyVerifier()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}

				if tc.wantErrMsg != "" && !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Errorf("error %v does not contain %q", err, tc.wantErrMsg)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if verifier == nil {
				t.Fatal("expected non-nil verifier")
			}
		})
	}
}

func TestBuildPolicy(t *testing.T) {
	t.Parallel()

	const validDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	pubKeyPath := writeECDSAPublicKey(t)

	cases := []struct {
		name    string
		digest  string
		wantErr bool
	}{
		{name: "valid key-based digest", digest: validDigest},
		{name: "malformed digest", digest: "notadigest", wantErr: true},
		{name: "invalid hex", digest: "sha256:ZZZZ", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v := &SigstoreVerifier{KeyPath: pubKeyPath}

			policy, err := v.buildPolicy(tc.digest)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if _, err := policy.BuildConfig(); err != nil {
				t.Fatalf("policy.BuildConfig: %v", err)
			}
		})
	}
}

func TestFetchBundleBlob(t *testing.T) { //nolint:funlen // table-driven test data
	t.Parallel()

	const bundleJSON = `{"mediaType":"application/vnd.dev.sigstore.bundle+json;version=0.3"}`

	blobDigest := godigest.FromString(bundleJSON)
	blobDesc := ocispec.Descriptor{
		MediaType: "application/vnd.dev.sigstore.bundle+json;version=0.3",
		Digest:    blobDigest,
		Size:      int64(len(bundleJSON)),
	}

	validManifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Layers:    []ocispec.Descriptor{blobDesc},
	}
	validManifestBytes, _ := json.Marshal(validManifest)
	validManifestDigest := godigest.FromBytes(validManifestBytes)

	emptyManifest := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest}
	emptyManifestBytes, _ := json.Marshal(emptyManifest)
	emptyManifestDigest := godigest.FromBytes(emptyManifestBytes)

	cases := []struct {
		name         string
		fetcher      *mockFetcher
		manifestDesc ocispec.Descriptor
		want         string
		wantErr      bool
		wantErrMsg   string
	}{
		{
			name: "success",
			fetcher: &mockFetcher{responses: map[godigest.Digest][]byte{
				validManifestDigest: validManifestBytes,
				blobDigest:          []byte(bundleJSON),
			}},
			manifestDesc: ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    validManifestDigest,
			},
			want: bundleJSON,
		},
		{
			name:    "manifest fetch error",
			fetcher: &mockFetcher{responses: map[godigest.Digest][]byte{}},
			manifestDesc: ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    godigest.FromString("missing"),
			},
			wantErr: true,
		},
		{
			name: "empty layers",
			fetcher: &mockFetcher{responses: map[godigest.Digest][]byte{
				emptyManifestDigest: emptyManifestBytes,
			}},
			manifestDesc: ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    emptyManifestDigest,
			},
			wantErr:    true,
			wantErrMsg: "no layers",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v := &SigstoreVerifier{}

			got, err := v.fetchBundleBlob(context.Background(), tc.fetcher, tc.manifestDesc)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}

				if tc.wantErrMsg != "" && !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Errorf("error %v does not contain %q", err, tc.wantErrMsg)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
