package ociconfigs

import (
	"context"
	"os"

	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// buildCredentialFunc returns an auth.CredentialFunc using the first matching
// source in priority order:
//
//  1. Explicit credentials set via WithCredentials or WithToken options.
//  2. OCI_USERNAME / OCI_PASSWORD environment variables.
//  3. OCI_TOKEN environment variable.
//  4. Docker config file (~/.docker/config.json).
//  5. Anonymous (no credentials).
func buildCredentialFunc(cfg Config) auth.CredentialFunc {
	// Priority 1: explicit credentials from functional options.
	if cfg.username != "" || cfg.password != "" {
		cred := auth.Credential{Username: cfg.username, Password: cfg.password}
		return staticCredential(cfg.Registry, cred)
	}
	if cfg.token != "" {
		cred := auth.Credential{AccessToken: cfg.token}
		return staticCredential(cfg.Registry, cred)
	}

	// Priority 2: environment variables.
	if username := os.Getenv(EnvUsername); username != "" {
		cred := auth.Credential{Username: username, Password: os.Getenv(EnvPassword)}
		return staticCredential(cfg.Registry, cred)
	}
	if token := os.Getenv(EnvToken); token != "" {
		cred := auth.Credential{AccessToken: token}
		return staticCredential(cfg.Registry, cred)
	}

	// Priority 3: docker config file.
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err == nil {
		return credentials.Credential(store)
	}

	// Priority 4: anonymous.
	return func(_ context.Context, _ string) (auth.Credential, error) {
		return auth.EmptyCredential, nil
	}
}

// staticCredential returns a CredentialFunc that supplies the given credential
// only when the requested registry host matches.
func staticCredential(registry string, cred auth.Credential) auth.CredentialFunc {
	return func(_ context.Context, hostport string) (auth.Credential, error) {
		if hostport == registry {
			return cred, nil
		}
		return auth.EmptyCredential, nil
	}
}
