package ociconfigs

import (
	"context"
	"testing"
)

func TestBuildCredentialFuncExplicitCredentials(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Registry: "registry.example.com",
		username: "alice",
		password: "s3cret",
	}
	fn := buildCredentialFunc(cfg)

	cred, err := fn(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if cred.Username != "alice" || cred.Password != "s3cret" {
		t.Fatalf("got username=%q password=%q", cred.Username, cred.Password)
	}
}

func TestBuildCredentialFuncExplicitToken(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Registry: "registry.example.com",
		token:    "tok-abc123",
	}
	fn := buildCredentialFunc(cfg)

	cred, err := fn(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if cred.AccessToken != "tok-abc123" {
		t.Fatalf("got access token %q", cred.AccessToken)
	}
}

func TestBuildCredentialFuncEnvUsername(t *testing.T) {
	t.Setenv(EnvUsername, "envuser")
	t.Setenv(EnvPassword, "envpass")
	// Ensure token env is clear so username wins.
	t.Setenv(EnvToken, "")

	cfg := Config{Registry: "registry.example.com"}
	fn := buildCredentialFunc(cfg)

	cred, err := fn(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if cred.Username != "envuser" || cred.Password != "envpass" {
		t.Fatalf("got username=%q password=%q", cred.Username, cred.Password)
	}
}

func TestBuildCredentialFuncEnvToken(t *testing.T) {
	t.Setenv(EnvUsername, "")
	t.Setenv(EnvToken, "env-token-xyz")

	cfg := Config{Registry: "registry.example.com"}
	fn := buildCredentialFunc(cfg)

	cred, err := fn(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if cred.AccessToken != "env-token-xyz" {
		t.Fatalf("got access token %q", cred.AccessToken)
	}
}

func TestBuildCredentialFuncExplicitBeatsEnv(t *testing.T) {
	// Explicit option must take priority over env var.
	t.Setenv(EnvUsername, "envuser")
	t.Setenv(EnvPassword, "envpass")

	cfg := Config{
		Registry: "registry.example.com",
		username: "explicit",
		password: "wins",
	}
	fn := buildCredentialFunc(cfg)

	cred, err := fn(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if cred.Username != "explicit" {
		t.Fatalf("expected explicit credentials, got username=%q", cred.Username)
	}
}

func TestBuildCredentialFuncWrongRegistry(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Registry: "registry.example.com",
		username: "alice",
		password: "pass",
	}
	fn := buildCredentialFunc(cfg)

	cred, err := fn(context.Background(), "other.registry.io")
	if err != nil {
		t.Fatal(err)
	}

	if cred.Username != "" || cred.Password != "" || cred.AccessToken != "" {
		t.Fatalf("expected empty credential for non-matching registry, got %+v", cred)
	}
}

func TestBuildCredentialFuncAnonymous(t *testing.T) {
	// Clear all auth env vars to exercise the anonymous fallback path.
	t.Setenv(EnvUsername, "")
	t.Setenv(EnvPassword, "")
	t.Setenv(EnvToken, "")

	cfg := Config{Registry: "registry.example.com"}
	fn := buildCredentialFunc(cfg)

	// Should not panic or error; result may be empty or docker-config-sourced.
	_, err := fn(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatal(err)
	}
}
