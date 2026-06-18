# oci-configs

`oci-configs` is a Go library for managing configuration files stored in an OCI
artifact registry. It provides a persistent local disk cache with automatic
staleness detection, flexible authentication, per-file local overrides, and
streaming access suitable for large files.

## Contents

- [Overview](#overview)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Building and pushing an OCI artifact](#building-and-pushing-an-oci-artifact)
- [Configuration](#configuration)
- [Authentication](#authentication)
- [Overrides](#overrides)
- [Progress and logging](#progress-and-logging)
- [Cache behaviour](#cache-behaviour)
- [Testing](#testing)
- [Signature Verification](#signature-verification)
- [Demo CLI](#demo-cli)

---

## Overview

The library pulls files from an OCI artifact registry (using
[ORAS](https://oras.land)), stores them in a local disk cache, and serves them
as `io.ReadCloser` streams. On every application start:

1. If the cache is **fresh** (younger than `MaxAge`), all files are present, and
   either no verifier is configured or the current digest has already been
   verified, files are served from disk with no network activity.
2. If the cache is **stale** (or fresh but unverified), the remote manifest
   digest is fetched. If the digest is unchanged the cache timestamp is
   refreshed; if it changed the files are re-downloaded.
3. If the **network is unavailable** but cached files exist, a warning is logged
   and the stale cache is used (`ErrStaleCacheUsed`).
4. If the network is unavailable and **no cache exists**, an error is returned.
5. If **signature verification is enabled** and the signature cannot be
   validated, `ErrVerificationFailed` is returned. This applies both after a
   fresh download and when verifying a previously-cached artifact whose digest
   has not yet been verified.

Per-file **local overrides** let application users substitute their own copies
of any file via a command-line flag, bypassing the cache entirely.

---

## Installation

```sh
go get github.com/robmyersrobmyers/oci-configs
```

Requires Go 1.26.2 or later.

---

## Quick start

```go
package main

import (
    "context"
    "io"
    "log"
    "log/slog"
    "os"

    ociconfigs "github.com/robmyersrobmyers/oci-configs"
)

var files = []ociconfigs.FileSpec{
    {Name: "schema",               Path: "schema.gql",                  MediaType: "application/graphql+text"},
    {Name: "persisted-op-manifest",Path: "persisted-op-manifest.json",  MediaType: "application/json"},
    {Name: "ast",                  Path: "ast.dat",                     MediaType: "application/octet-stream"},
    {Name: "config",               Path: "config.yaml",                 MediaType: "application/yaml"},
}

func main() {
    ctx := context.Background()

    client, err := ociconfigs.New(ociconfigs.Config{
        Registry:   "registry.example.com",
        Repository: "myorg/configs",
        Tag:        "latest",
        Files:      files,
    },
        ociconfigs.WithLogger(slog.Default()),
        ociconfigs.WithCredentials(os.Getenv("REGISTRY_USER"), os.Getenv("REGISTRY_PASS")),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer func() { _ = client.Close() }()

    // Download / validate the cache at startup.
    if err := client.Prefetch(ctx); err != nil && err != ociconfigs.ErrStaleCacheUsed {
        log.Fatal(err)
    }

    // Read a file — always call Close() when done.
    rc, err := client.Get(ctx, "schema")
    if err != nil {
        log.Fatal(err)
    }
    defer func() { _ = rc.Close() }()

    _, _ = io.Copy(os.Stdout, rc)
}
```

### Reading from environment variables

```go
client, err := ociconfigs.FromEnv(files,
    ociconfigs.WithLogger(slog.Default()),
)
```

`FromEnv` reads `OCI_REGISTRY`, `OCI_REPOSITORY`, `OCI_TAG`, `OCI_USERNAME`,
`OCI_PASSWORD`, `OCI_TOKEN`, `OCI_CACHE_DIR`, `OCI_MAX_AGE`, and the four
`OCI_VERIFY_SIGSTORE_*` variables. Signature verification is enabled
automatically when at least one of `OCI_VERIFY_SIGSTORE_KEY`,
`OCI_VERIFY_SIGSTORE_CERT_IDENTITY`, or `OCI_VERIFY_SIGSTORE_CERT_ISSUER` is
set.

---

## Building and pushing an OCI artifact

Files are stored as plain ORAS artifacts.

### Install the ORAS CLI

Visit [oras.land](https://oras.land/docs/installation) for installation instructions or install via a package manager.

```sh
brew install oras
```

### Push your files

```sh
oras push registry.example.com/myorg/configs:latest \
  schema.gql:application/graphql+text \
  persisted-op-manifest.json:application/json \
  ast.dat:application/octet-stream \
  config.yaml:application/yaml
```

Each file becomes an independently-addressable blob in the registry. The
`Path` field in `FileSpec` must match the filename used in `oras push`.

### Authentication for push

Authenticate before pushing.

```sh
oras login registry.example.com -u myuser -p mypass
```

or

```sh
echo "$PASS" | oras login registry.example.com -u myuser --password-stdin
```

---

## Configuration

### `Config` struct

| Field        | Type                    | Default                              | Description |
|--------------|-------------------------|--------------------------------------|-------------|
| `Registry`   | `string`                | —                                    | Registry hostname, e.g. `registry.example.com` |
| `Repository` | `string`                | —                                    | Repository path, e.g. `myorg/configs` |
| `Tag`        | `string`                | —                                    | Tag or digest, e.g. `latest` |
| `Files`      | `[]FileSpec`            | —                                    | Files to manage (at least one required) |
| `CacheDir`   | `string`                | `os.UserCacheDir()/oci-configs`      | Local cache directory |
| `MaxAge`     | `time.Duration`         | `168h` (7 days)                      | Cache freshness TTL |
| `Overrides`  | `map[string]string`     | —                                    | `name → local file path` overrides |
| `OnProgress` | `ProgressFunc`          | `nil`                                | Per-file download progress callback |
| `Logger`     | `*slog.Logger`          | `nil`                                | Structured logger |

### `FileSpec` struct

| Field       | Type     | Description |
|-------------|----------|-------------|
| `Name`      | `string` | Logical name used with `Get`, e.g. `"schema"` |
| `Path`      | `string` | File name in the OCI artifact, e.g. `"schema.gql"` |
| `MediaType` | `string` | OCI media type, e.g. `"application/json"` |

### `SigstoreVerifier` struct

| Field | Type | Description |
|---|---|---|
| `KeyPath` | `string` | Path to a PEM-encoded ECDSA or Ed25519 public key. Mutually exclusive with `CertIdentity`/`CertOIDCIssuer`. |
| `CertIdentity` | `string` | Expected SAN in the Fulcio signing certificate (keyless). Must pair with `CertOIDCIssuer`. |
| `CertOIDCIssuer` | `string` |  Expected OIDC issuer URL for keyless verification. |
| `RequireRekor` | `bool` | `false` | Require a Rekor transparency-log inclusion proof. |
| `RegistryUsername` | `string` |  Username for fetching signature bundles. Falls back to `OCI_USERNAME`. |
| `RegistryPassword` | `string` |  Password for fetching signature bundles. Falls back to `OCI_PASSWORD`. |
| `RegistryToken` | `string` |  Bearer token for fetching signature bundles. Falls back to `OCI_TOKEN`. |

### Environment variables

| Variable       | Equivalent option / field | Example |
|----------------|--------------------------|---------|
| `OCI_REGISTRY`   | `Config.Registry`       | `registry.example.com` |
| `OCI_REPOSITORY` | `Config.Repository`     | `myorg/configs` |
| `OCI_TAG`        | `Config.Tag`            | `latest` |
| `OCI_USERNAME`   | `WithCredentials`       | `myuser` |
| `OCI_PASSWORD`   | `WithCredentials`       | `mypass` |
| `OCI_TOKEN`      | `WithToken`             | `ghp_abc...` |
| `OCI_CACHE_DIR`  | `WithCacheDir`          | `/var/cache/myapp` |
| `OCI_MAX_AGE`    | `WithMaxAge`            | `48h` |
| `OCI_VERIFY_SIGSTORE_KEY` | `SigstoreVerifier.KeyPath` | `/etc/keys/cosign.pub` |
| `OCI_VERIFY_SIGSTORE_CERT_IDENTITY` | `SigstoreVerifier.CertIdentity` | `https://github.com/myorg/myrepo/.github/workflows/release.yml@refs/heads/main` |
| `OCI_VERIFY_SIGSTORE_CERT_ISSUER` | `SigstoreVerifier.CertOIDCIssuer` | `https://token.actions.githubusercontent.com` |
| `OCI_VERIFY_SIGSTORE_REQUIRE_REKOR` | `SigstoreVerifier.RequireRekor` | `true` |

Functional options passed to `New` or `FromEnv` take priority over environment
variables.

### Functional options

```go
ociconfigs.WithCacheDir("/var/cache/myapp")
ociconfigs.WithMaxAge(48 * time.Hour)
ociconfigs.WithCredentials("user", "pass")
ociconfigs.WithToken("ghp_abc123")
ociconfigs.WithOverride("schema", "/local/schema.gql")
ociconfigs.WithProgress(myProgressFunc)
ociconfigs.WithLogger(slog.Default())
ociconfigs.WithVerifier(&ociconfigs.SigstoreVerifier{KeyPath: "/etc/keys/cosign.pub"})
```

---

## Authentication

Credentials are resolved in priority order:

1. `WithCredentials(username, password)` or `WithToken(token)` option.
2. `OCI_USERNAME` / `OCI_PASSWORD` environment variables.
3. `OCI_TOKEN` environment variable.
4. Docker config file (`~/.docker/config.json`) — populated by `docker login`
   or `oras login`.
5. Anonymous (no credentials).

### Private registry example

```sh
# One-time login (writes to ~/.docker/config.json)
oras login registry.example.com -u myuser -p mypass

# Application reads credentials automatically via docker config
client, err := ociconfigs.New(cfg)
```

### Environment variable example (CI/CD)

```sh
export OCI_USERNAME=ci-bot
export OCI_PASSWORD=$REGISTRY_TOKEN
```

```go
client, err := ociconfigs.FromEnv(files)
```

---

## Overrides

Any file can be replaced with a local path at runtime. This is the recommended
pattern for wiring up command-line flags:

```go
// In your cobra/flag setup:
var schemaOverride string
flag.StringVar(&schemaOverride, "schema-file", "", "override: local schema file path")
flag.Parse()

var opts []ociconfigs.Option
if schemaOverride != "" {
    opts = append(opts, ociconfigs.WithOverride("schema", schemaOverride))
}

client, err := ociconfigs.New(cfg, opts...)
```

When an override is set, `Get(ctx, "schema")` opens the local file directly —
no cache or network access occurs for that file.

---

## Progress and logging

### Progress callback

The `OnProgress` callback is called after every read chunk during a download.
Use it to drive a progress bar for interactive CLIs:

```go
import "github.com/schollz/progressbar/v3"

bars := map[string]*progressbar.ProgressBar{}

ociconfigs.WithProgress(func(ev ociconfigs.ProgressEvent) {
    bar, ok := bars[ev.Name]
    if !ok {
        bar = progressbar.DefaultBytes(ev.Total, ev.Name)
        bars[ev.Name] = bar
    }
    _ = bar.Set64(ev.Completed)
    if ev.Done {
        _ = bar.Finish()
    }
})
```

Example output:
```
schema.gql          [=========>          ] 45%  270MB/600MB  12.3 MB/s  ETA 27s
```

### Structured logging

Pass a `*slog.Logger` for headless/server environments:

```go
ociconfigs.WithLogger(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
```

Example output:
```json
{"time":"2026-04-09T10:23:45Z","level":"INFO","msg":"downloading artifact","registry":"registry.example.com","repository":"myorg/configs","tag":"latest"}
{"time":"2026-04-09T10:24:09Z","level":"WARN","msg":"remote check failed; using stale cache","err":"connection refused"}
```

---

## Cache behaviour

### Directory layout

```
~/.cache/oci-configs/
  registry.example.com_myorg_configs_latest/
    manifest.json             ← {"digest":"sha256:…","cached_at":"2026-04-09T10:00:00Z"}
    schema.gql
    persisted-op-manifest.json
    ast.dat
    config.yaml
```

The cache directory key is derived from `registry/repository:tag` with `/` and
`:` replaced by `_`.

### Staleness algorithm

```
Prefetch():
  if cache is fresh AND all files present AND (no verifier OR digest verified)
    → return immediately (no network)
  else
    fetch remote manifest digest (HEAD request, minimal bandwidth)
    if network error and files cached → log warning, return ErrStaleCacheUsed
    if network error and no cache   → return ErrNoCache (wrapping the cause)
    if remote digest == cached digest
      if verifier configured AND digest not yet verified
        → verify signature; on failure return ErrVerificationFailed
      → update cached_at, return
    if remote digest differs
      → re-download all files
      → if verifier configured: verify signature; on failure return ErrVerificationFailed
      → update cache with verified_digest
```

### Inspecting the cache

```go
status, err := client.CacheInfo()
fmt.Println(status.CacheDir)
fmt.Println(status.Digest)
fmt.Println(status.CachedAt)
fmt.Println(status.Fresh)
for _, f := range status.Files {
    fmt.Printf("%s: %d bytes\n", f.Name, f.Size)
}
```

---

## Testing

Unit tests run with no network access and no registry:

```sh
go test ./...
```

The `fetchDigest` and `download` functions on `Client` are injectable fields,
allowing test code to replace them with stubs:

```go
client.fetchDigest = func(ctx context.Context, cfg Config) (string, error) {
    return "sha256:abc", nil
}
client.download = func(ctx context.Context, cfg Config, cm *cacheManager) error {
    return cm.writeManifest("sha256:abc", "")
}
```

Signature verification is also injectable: `ArtifactVerifier` is an interface
with a single `Verify` method, so any struct that implements it can stand in for
a real `SigstoreVerifier`:

```go
type stubVerifier struct{ err error }

func (s *stubVerifier) Verify(_ context.Context, _, _, _ string) error { return s.err }

client.cfg.Verifier = &stubVerifier{} // passes verification

client.cfg.Verifier = &stubVerifier{err: errors.New("bad signature")} // fails verification
```

---

## Signature Verification

Signatures stored using the OCI Referrers API can be optionally verified using [Sigstore](https://www.sigstore.dev).
Verification runs after each download; the local cache is not updated if the signature is invalid.

Pass a `SigstoreVerifier` via `WithVerifier`:

```go
client, err := ociconfigs.New(cfg,
    ociconfigs.WithVerifier(&ociconfigs.SigstoreVerifier{
        KeyPath: "/etc/keys/cosign.pub",
    }),
)
```

### Key-based

Key based verification is recommended for private or airgapped registries.

```go
ociconfigs.WithVerifier(&ociconfigs.SigstoreVerifier{
    KeyPath: "/etc/keys/cosign.pub",
})
```

Contacts only the OCI registry — no Rekor, no Fulcio, no TUF network access required.

### Keyless

Keyless verification requires an OIDC issuer like GitHub Actions, Google Workload Identity, etc.

```go
ociconfigs.WithVerifier(&ociconfigs.SigstoreVerifier{
    CertIdentity:   "https://github.com/myorg/myrepo/.github/workflows/release.yml@refs/heads/main",
    CertOIDCIssuer: "https://token.actions.githubusercontent.com",
    RequireRekor:   true,
})
```

Requires outbound access to TUF mirrors. With `RequireRekor: true`, a Rekor inclusion proof provides a durable timestamp independent of the short-lived Fulcio certificate.

### Configuration via environment variables

```go
ociconfigs.WithVerifier(ociconfigs.SigstoreVerifierFromEnv())
```

| Variable | Purpose |
|---|---|
| `OCI_VERIFY_SIGSTORE_KEY` | Path to PEM-encoded _public_ key (key-based) |
| `OCI_VERIFY_SIGSTORE_CERT_IDENTITY` | Fulcio certificate SAN (keyless) |
| `OCI_VERIFY_SIGSTORE_CERT_ISSUER` | OIDC issuer URL (keyless) |
| `OCI_VERIFY_SIGSTORE_REQUIRE_REKOR` | Require Rekor inclusion proof (`true`/`1`/`yes`) |

Signature bundles are fetched using the same `OCI_USERNAME`, `OCI_PASSWORD`, and `OCI_TOKEN` variables as the rest of the library.

---

## Demo CLI

A small CLI is included for testing and exploration:

```sh
go install github.com/robmyersrobmyers/oci-configs/cmd/oci-configs@latest
```

```sh
export OCI_REGISTRY=registry.example.com
export OCI_REPOSITORY=myorg/configs
export OCI_TAG=latest
export OCI_USERNAME=myuser
export OCI_PASSWORD=mypass

# Download all files
oci-configs pull

# Download and verify signature with a PEM public key (key-based)
oci-configs pull --verify-key=/etc/keys/cosign.pub

# Download and verify signature with a keyless identity (GitHub Actions)
oci-configs pull \
  --verify-cert-identity=https://github.com/myorg/myrepo/.github/workflows/release.yml@refs/heads/main \
  --verify-cert-issuer=https://token.actions.githubusercontent.com \
  --verify-require-rekor

# Stream a file to stdout
oci-configs get --name=schema > schema.gql

# Use a local file instead of the cached one
oci-configs get --name=config --override-config=/local/config.yaml

# Inspect the cache
oci-configs cache-info
oci-configs cache-info --json
```

Verification flags can also be supplied via environment variables — see
[Signature Verification](#configuration-via-environment-variables) for the full list.
