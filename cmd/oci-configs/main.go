// Command oci-configs is a demonstration CLI for the ociconfigs package.
// It shows how to wire OCI_* environment variables and command-line flags
// to pull, inspect, and read files from an OCI artifact registry.
//
// Usage:
//
//	oci-configs pull                     # download/refresh all configured files
//	oci-configs pull --verify-key=cosign.pub  # pull and verify signature
//	oci-configs get --name=schema        # print a file to stdout
//	oci-configs cache-info               # show cache metadata
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	ociconfigs "github.com/robmyersrobmyers/oci-configs"
)

var (
	errSubcommandRequired = errors.New("subcommand required")
	errUnknownSubcommand  = errors.New("unknown subcommand")
	errNameRequired       = errors.New("--name is required")
)

// demoFiles mirrors the four files used in the reference OCI artifact.
// In a real application, callers define their own []ociconfigs.FileSpec.
var demoFiles = []ociconfigs.FileSpec{ //nolint:gochecknoglobals
	{Name: "schema", Path: "schema.gql", MediaType: "application/graphql+text"},
	{Name: "persisted-op-manifest", Path: "persisted-op-manifest.json", MediaType: "application/json"},
	{Name: "ast", Path: "ast.dat", MediaType: "application/octet-stream"},
	{Name: "config", Path: "config.yaml", MediaType: "application/yaml"},
}

func main() {
	err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		printUsage()

		return errSubcommandRequired
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "pull":
		return runPull(ctx, os.Args[2:])
	case "get":
		return runGet(ctx, os.Args[2:])
	case "cache-info":
		return runCacheInfo(os.Args[2:])
	default:
		printUsage()

		return fmt.Errorf("%w: %q", errUnknownSubcommand, os.Args[1])
	}
}

type globalFlags struct {
	registry   string
	repository string
	tag        string
	cacheDir   string
	maxAge     string
	username   string
	password   string
	token      string
	verbose    bool
}

// verifyFlags holds Sigstore signature-verification options for the pull subcommand.
type verifyFlags struct {
	keyPath      string
	certIdentity string
	certIssuer   string
	requireRekor bool
}

func (v *verifyFlags) register(fs *flag.FlagSet) {
	v.requireRekor, _ = strconv.ParseBool(os.Getenv(ociconfigs.EnvVerifySigStoreRequireRekor))

	fs.StringVar(&v.keyPath, "verify-key",
		os.Getenv(ociconfigs.EnvVerifySigStoreKeyPath),
		"PEM public key for key-based signature verification (env: OCI_VERIFY_SIGSTORE_KEY)")
	fs.StringVar(&v.certIdentity, "verify-cert-identity",
		os.Getenv(ociconfigs.EnvVerifySigStoreCertIdentity),
		"Fulcio certificate SAN for keyless verification (env: OCI_VERIFY_SIGSTORE_CERT_IDENTITY)")
	fs.StringVar(&v.certIssuer, "verify-cert-issuer",
		os.Getenv(ociconfigs.EnvVerifySigStoreCertIssuer),
		"OIDC issuer URL for keyless verification (env: OCI_VERIFY_SIGSTORE_CERT_ISSUER)")
	fs.BoolVar(&v.requireRekor, "verify-require-rekor", v.requireRekor,
		"require Rekor inclusion proof (env: OCI_VERIFY_SIGSTORE_REQUIRE_REKOR)")
}

// option returns a WithVerifier option if any verification flag is set, or nil.
func (v *verifyFlags) option() ociconfigs.Option {
	if v.keyPath == "" && v.certIdentity == "" && v.certIssuer == "" {
		return nil
	}

	return ociconfigs.WithVerifier(&ociconfigs.SigstoreVerifier{
		KeyPath:        v.keyPath,
		CertIdentity:   v.certIdentity,
		CertOIDCIssuer: v.certIssuer,
		RequireRekor:   v.requireRekor,
	})
}

func (g *globalFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.registry, "registry", os.Getenv(ociconfigs.EnvRegistry), "OCI registry hostname (env: OCI_REGISTRY)")
	fs.StringVar(&g.repository, "repository",
		os.Getenv(ociconfigs.EnvRepository), "OCI repository path (env: OCI_REPOSITORY)")
	fs.StringVar(&g.tag, "tag", os.Getenv(ociconfigs.EnvTag), "image tag or digest (env: OCI_TAG)")
	fs.StringVar(&g.cacheDir, "cache-dir", os.Getenv(ociconfigs.EnvCacheDir), "cache directory (env: OCI_CACHE_DIR)")
	fs.StringVar(&g.maxAge, "max-age", os.Getenv(ociconfigs.EnvMaxAge), "max cache age, e.g. 168h (env: OCI_MAX_AGE)")
	fs.StringVar(&g.username, "username", "", "registry username (env: OCI_USERNAME)")
	fs.StringVar(&g.password, "password", "", "registry password (env: OCI_PASSWORD)")
	fs.StringVar(&g.token, "token", "", "registry bearer token (env: OCI_TOKEN)")
	fs.BoolVar(&g.verbose, "verbose", false, "enable verbose logging")
}

func (g *globalFlags) buildClient(
	files []ociconfigs.FileSpec, extraOpts ...ociconfigs.Option,
) (*ociconfigs.Client, error) {
	cfg := ociconfigs.Config{
		Registry:   g.registry,
		Repository: g.repository,
		Tag:        g.tag,
		Files:      files,
	}

	var opts []ociconfigs.Option

	if g.cacheDir != "" {
		opts = append(opts, ociconfigs.WithCacheDir(g.cacheDir))
	}

	if g.maxAge != "" {
		d, err := time.ParseDuration(g.maxAge)
		if err != nil {
			return nil, fmt.Errorf("--max-age %q: %w", g.maxAge, err)
		}

		opts = append(opts, ociconfigs.WithMaxAge(d))
	}

	if g.username != "" || g.password != "" {
		opts = append(opts, ociconfigs.WithCredentials(g.username, g.password))
	}

	if g.token != "" {
		opts = append(opts, ociconfigs.WithToken(g.token))
	}

	logLevel := slog.LevelInfo
	if g.verbose {
		logLevel = slog.LevelDebug
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	opts = append(opts, ociconfigs.WithLogger(logger))
	opts = append(opts, ociconfigs.WithProgress(stderrProgress))
	opts = append(opts, extraOpts...)

	client, err := ociconfigs.New(cfg, opts...)
	if err != nil {
		return nil, fmt.Errorf("building client: %w", err)
	}

	return client, nil
}

func runPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	gf := &globalFlags{}
	gf.register(fs)

	vf := &verifyFlags{}
	vf.register(fs)

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing pull flags: %w", err)
	}

	var extraOpts []ociconfigs.Option
	if opt := vf.option(); opt != nil {
		extraOpts = append(extraOpts, opt)
	}

	client, err := gf.buildClient(demoFiles, extraOpts...)
	if err != nil {
		return err
	}

	defer func() { _ = client.Close() }()

	if err := client.Prefetch(ctx); err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	return nil
}

func runGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	gf := &globalFlags{}
	gf.register(fs)

	var name string

	fs.StringVar(&name, "name", "", "logical file name to retrieve (required)")

	// Per-file local path overrides.
	overridePtrs := make(map[string]*string, len(demoFiles))

	for _, f := range demoFiles {
		v := fs.String("override-"+f.Name, "", "local file path override for "+f.Name)
		overridePtrs[f.Name] = v
	}

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing get flags: %w", err)
	}

	if name == "" {
		return errNameRequired
	}

	var overrideOpts []ociconfigs.Option

	for n, p := range overridePtrs {
		if *p != "" {
			overrideOpts = append(overrideOpts, ociconfigs.WithOverride(n, *p))
		}
	}

	client, err := gf.buildClient(demoFiles, overrideOpts...)
	if err != nil {
		return err
	}

	defer func() { _ = client.Close() }()

	rc, err := client.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("get %q: %w", name, err)
	}

	defer func() { _ = rc.Close() }()

	if _, err = io.Copy(os.Stdout, rc); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	return nil
}

func runCacheInfo(args []string) error {
	fs := flag.NewFlagSet("cache-info", flag.ExitOnError)
	gf := &globalFlags{}
	gf.register(fs)

	var asJSON bool

	fs.BoolVar(&asJSON, "json", false, "output as JSON")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing cache-info flags: %w", err)
	}

	client, err := gf.buildClient(demoFiles)
	if err != nil {
		return err
	}

	defer func() { _ = client.Close() }()

	status, err := client.CacheInfo()
	if err != nil {
		return fmt.Errorf("cache-info: %w", err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		if err := enc.Encode(status); err != nil {
			return fmt.Errorf("encoding JSON: %w", err)
		}

		return nil
	}

	return printCacheStatusTable(status)
}

func printCacheStatusTable(status ociconfigs.CacheStatus) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintf(w, "Cache directory:\t%s\n", status.CacheDir)

	if status.Digest != "" {
		_, _ = fmt.Fprintf(w, "Digest:\t%s\n", status.Digest)
		_, _ = fmt.Fprintf(w, "Cached at:\t%s\n", status.CachedAt.Format(time.RFC3339))
		_, _ = fmt.Fprintf(w, "Fresh:\t%v\n", status.Fresh)
	} else {
		_, _ = fmt.Fprintf(w, "Manifest:\t(none — not yet downloaded)\n")
	}

	_, _ = fmt.Fprintf(w, "\nFile\tPath\tSize\n")

	for _, entry := range status.Files {
		sizeStr := "(missing)"
		if entry.Size > 0 {
			sizeStr = fmt.Sprintf("%d bytes", entry.Size)
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", entry.Name, entry.Path, sizeStr)
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("flushing output: %w", err)
	}

	return nil
}

func stderrProgress(ev ociconfigs.ProgressEvent) {
	if ev.Err != nil {
		fmt.Fprintf(os.Stderr, "\r[ERROR] %-40s %v\n", ev.Name, ev.Err)

		return
	}

	if ev.Done {
		fmt.Fprintf(os.Stderr, "\r[done]  %-40s %d bytes\n", ev.Name, ev.Completed)

		return
	}

	if ev.Total > 0 {
		pct := ev.Completed * 100 / ev.Total
		fmt.Fprintf(os.Stderr, "\r[%3d%%]  %-40s %d / %d bytes", pct, ev.Name, ev.Completed, ev.Total)
	} else {
		fmt.Fprintf(os.Stderr, "\r        %-40s %d bytes", ev.Name, ev.Completed)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: oci-configs <subcommand> [flags]

Subcommands:
  pull        Download/refresh all configured files from the OCI registry.
  get         Stream a single file to stdout.
  cache-info  Show local cache metadata.

All subcommands accept these flags (env var fallback shown):
  --registry      OCI registry hostname          (OCI_REGISTRY)
  --repository    OCI repository path            (OCI_REPOSITORY)
  --tag           Image tag or digest            (OCI_TAG)
  --cache-dir     Local cache directory          (OCI_CACHE_DIR)
  --max-age       Cache TTL, e.g. 168h           (OCI_MAX_AGE)
  --username      Registry username              (OCI_USERNAME)
  --password      Registry password              (OCI_PASSWORD)
  --token         Registry bearer token          (OCI_TOKEN)
  --verbose       Enable debug logging

The 'pull' subcommand also accepts signature verification flags:
  --verify-key              PEM public key path for key-based verification
                            (OCI_VERIFY_SIGSTORE_KEY)
  --verify-cert-identity    Fulcio certificate SAN for keyless verification
                            (OCI_VERIFY_SIGSTORE_CERT_IDENTITY)
  --verify-cert-issuer      OIDC issuer URL for keyless verification
                            (OCI_VERIFY_SIGSTORE_CERT_ISSUER)
  --verify-require-rekor    Require Rekor inclusion proof (default: false)
                            (OCI_VERIFY_SIGSTORE_REQUIRE_REKOR)

The 'get' subcommand also accepts:
  --name                           Logical file name to retrieve (required).
  --override-schema                Use a local file instead of the cached artifact.
  --override-persisted-op-manifest Use a local file instead of the cached artifact.
  --override-ast                   Use a local file instead of the cached artifact.
  --override-config                Use a local file instead of the cached artifact.

Examples:
  export OCI_REGISTRY=registry.example.com
  export OCI_REPOSITORY=myorg/configs
  export OCI_TAG=latest
  export OCI_USERNAME=myuser
  export OCI_PASSWORD=mypass

  oci-configs pull
  oci-configs pull --verify-key=/etc/keys/cosign.pub
  oci-configs get --name=schema > schema.gql
  oci-configs cache-info --json
  oci-configs get --name=config --override-config=/local/config.yaml
`)
}
