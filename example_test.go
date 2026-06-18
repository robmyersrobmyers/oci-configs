package ociconfigs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	ociconfigs "github.com/robmyersrobmyers/oci-configs"
)

var exampleFiles = []ociconfigs.FileSpec{
	{Name: "schema", Path: "schema.gql", MediaType: "application/graphql+text"},
	{Name: "config", Path: "config.yaml", MediaType: "application/yaml"},
}

// ExampleNew demonstrates creating a Client with an explicit Config.
func ExampleNew() {
	client, err := ociconfigs.New(ociconfigs.Config{
		Registry:   "registry.example.com",
		Repository: "myorg/configs",
		Tag:        "latest",
		Files:      exampleFiles,
	})
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	if err := client.Prefetch(context.Background()); err != nil && !errors.Is(err, ociconfigs.ErrStaleCacheUsed) {
		return
	}
}

// ExampleFromEnv demonstrates creating a Client entirely from OCI_* environment
// variables, which is the recommended approach for production deployments.
func ExampleFromEnv() {
	// Reads OCI_REGISTRY, OCI_REPOSITORY, OCI_TAG, OCI_USERNAME/OCI_PASSWORD
	// (or OCI_TOKEN), OCI_CACHE_DIR, and OCI_MAX_AGE from the environment.
	client, err := ociconfigs.FromEnv(exampleFiles)
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	if err := client.Prefetch(context.Background()); err != nil && !errors.Is(err, ociconfigs.ErrStaleCacheUsed) {
		return
	}
}

// ExampleClient_Prefetch demonstrates the recommended startup pattern: warm the
// cache once at application start so that subsequent Get calls are served from
// disk without network round-trips.
func ExampleClient_Prefetch() {
	client, err := ociconfigs.New(ociconfigs.Config{
		Registry:   "registry.example.com",
		Repository: "myorg/configs",
		Tag:        "stable",
		Files:      exampleFiles,
	})
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	err = client.Prefetch(context.Background())

	switch {
	case err == nil:
		// Cache is up to date.
	case errors.Is(err, ociconfigs.ErrStaleCacheUsed):
		// Registry was unreachable; the existing cache will be used.
		fmt.Fprintln(os.Stderr, "warning: using stale cache")
	default:
		// No cache and no network — cannot proceed.
		return
	}
}

// ExampleClient_Get demonstrates retrieving a named file and reading its
// contents. The caller must close the returned ReadCloser.
func ExampleClient_Get() {
	client, err := ociconfigs.New(ociconfigs.Config{
		Registry:   "registry.example.com",
		Repository: "myorg/configs",
		Tag:        "latest",
		Files:      exampleFiles,
	})
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	rc, err := client.Get(context.Background(), "schema")
	if err != nil {
		return
	}

	defer func() { _ = rc.Close() }()

	_, err = io.Copy(os.Stdout, rc)
	if err != nil {
		return
	}
}

// ExampleClient_CacheInfo demonstrates inspecting the local cache state without
// making any network calls.
func ExampleClient_CacheInfo() {
	client, err := ociconfigs.New(ociconfigs.Config{
		Registry:   "registry.example.com",
		Repository: "myorg/configs",
		Tag:        "latest",
		Files:      exampleFiles,
	})
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	status, err := client.CacheInfo()
	if err != nil {
		return
	}

	fmt.Printf("cache dir: %s\n", status.CacheDir)
	fmt.Printf("digest:    %s\n", status.Digest)
	fmt.Printf("cached at: %s\n", status.CachedAt.Format(time.RFC3339))
	fmt.Printf("fresh:     %v\n", status.Fresh)

	for _, f := range status.Files {
		fmt.Printf("  %-30s %d bytes\n", f.Name, f.Size)
	}
}

// ExampleWithProgress demonstrates attaching a progress callback to receive
// per-file download updates. The callback may be invoked from multiple
// goroutines concurrently.
func ExampleWithProgress() {
	progress := func(e ociconfigs.ProgressEvent) {
		switch {
		case e.Err != nil:
			_, _ = fmt.Fprintf(os.Stderr, "%s: error: %v\n", e.Name, e.Err)
		case e.Done:
			_, _ = fmt.Fprintf(os.Stderr, "%s: done\n", e.Name)
		case e.Total > 0:
			pct := float64(e.Completed) / float64(e.Total) * 100
			_, _ = fmt.Fprintf(os.Stderr, "%s: %.0f%%\n", e.Name, pct)
		}
	}

	client, err := ociconfigs.New(
		ociconfigs.Config{
			Registry:   "registry.example.com",
			Repository: "myorg/configs",
			Tag:        "latest",
			Files:      exampleFiles,
		},
		ociconfigs.WithProgress(progress),
	)
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	_ = client.Prefetch(context.Background())
}

// ExampleWithLogger demonstrates attaching a structured slog.Logger to receive
// diagnostic output from the client.
func ExampleWithLogger() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	client, err := ociconfigs.New(
		ociconfigs.Config{
			Registry:   "registry.example.com",
			Repository: "myorg/configs",
			Tag:        "latest",
			Files:      exampleFiles,
		},
		ociconfigs.WithLogger(logger),
	)
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	_ = client.Prefetch(context.Background())
}

// ExampleWithOverride demonstrates substituting a local file for a named
// artifact file. This is useful during development and testing to avoid
// registry dependencies.
func ExampleWithOverride() {
	client, err := ociconfigs.New(
		ociconfigs.Config{
			Registry:   "registry.example.com",
			Repository: "myorg/configs",
			Tag:        "latest",
			Files:      exampleFiles,
		},
		ociconfigs.WithOverride("schema", "/path/to/local/schema.gql"),
	)
	if err != nil {
		return
	}

	defer func() { _ = client.Close() }()

	// Get("schema") opens /path/to/local/schema.gql instead of the cache.
	rc, err := client.Get(context.Background(), "schema")
	if err != nil {
		return
	}

	defer func() { _ = rc.Close() }()
}
