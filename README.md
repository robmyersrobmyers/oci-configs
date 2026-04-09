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
- [Demo CLI](#demo-cli)

---

## Overview

The library pulls files from an OCI artifact registry (using
[ORAS](https://oras.land)), stores them in a local disk cache, and serves them
as `io.ReadCloser` streams. On every application start:

1. If the cache is **fresh** (younger than `MaxAge`), files are served from disk
   with no network activity.
2. If the cache is **stale**, the remote manifest digest is fetched. If the
   digest is unchanged the cache timestamp is refreshed; if it changed the files
   are re-downloaded.
3. If the **network is unavailable** but cached files exist, a warning is logged
   and the stale cache is used (`ErrStaleCacheUsed`).
4. If the network is unavailable and **no cache exists**, an error is returned.

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
`OCI_PASSWORD`, `OCI_TOKEN`, `OCI_CACHE_DIR`, and `OCI_MAX_AGE`.

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
  if cache is fresh (cached_at + MaxAge > now) and all files present
    → return immediately (no network)
  else
    fetch remote manifest digest (HEAD request, minimal bandwidth)
    if network error and files cached → log warning, return ErrStaleCacheUsed
    if network error and no cache   → return ErrNoCache (wrapping the cause)
    if remote digest == cached digest → update cached_at, return
    if remote digest differs         → re-download all files
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
    return cm.writeManifest("sha256:abc")
}
```

Integration tests that hit a real registry require the `integration` build tag
and the `OCI_*` environment variables to be set:

```sh
go test -tags integration ./...
```

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

# Stream a file to stdout
oci-configs get --name=schema > schema.gql

# Use a local file instead of the cached one
oci-configs get --name=config --override-config=/local/config.yaml

# Inspect the cache
oci-configs cache-info
oci-configs cache-info --json
```
