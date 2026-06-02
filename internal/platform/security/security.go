// Package security is the abstraction layer for secret retrieval. Today
// only EnvSecretsProvider exists (reads os.Getenv). When Vault arrives,
// it will be a second SecretsProvider implementation — the composition
// root picks which one to use; everything else (config.ResolveSecrets,
// callers fetching by name) stays untouched.
//
// Design choice: thin interface, no caching or batching at this layer.
// A Vault-backed implementation can add its own cache internally if
// needed; the abstraction stays trivial.
package security

import (
	"context"
	"fmt"
	"os"
)

// SecretsProvider is the abstraction the rest of the platform speaks.
// Implementations may block on a remote call (Vault), read a local file
// or hit a process-level cache — callers are agnostic.
type SecretsProvider interface {
	Get(ctx context.Context, name string) (string, error)
}

// EnvSecretsProvider resolves secrets from environment variables. Useful
// for local development and for production deployments where secrets are
// injected by the orchestrator (k8s secret → env var). Not for the
// long-term goal of "no secrets in env" — that's what Vault is for.
type EnvSecretsProvider struct{}

func (p *EnvSecretsProvider) Get(_ context.Context, name string) (string, error) {
	val, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("secret %q not present in environment", name)
	}
	return val, nil
}
