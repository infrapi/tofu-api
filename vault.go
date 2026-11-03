package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	infraerrors "github.com/infrapi/lib/pkg/errors"
	infravault "github.com/infrapi/lib/pkg/vault"
)

// namespaceSecrets reads each namespace's provider configuration from a KV v2
// secret in Vault, logged in with an AppRole: the service holds one
// identity, allowed to read those secrets, and the file names no namespace.
// The client keeps its token valid on its own.
type namespaceSecrets struct {
	client *infravault.Client
	mount  string
	path   string
}

// newNamespaceSecrets logs in: a login refused is a mistake in the
// configuration, better told at startup than at each namespace's first
// request. retries is the client's RetryCount, 0 for the lib's default.
func newNamespaceSecrets(ctx context.Context, cfg vaultConfig, retries int) (*namespaceSecrets, error) {
	client, err := infravault.NewClient(infravault.ClientOptions{
		Address: cfg.Address, CACertPath: cfg.CACert, RetryCount: retries,
	})
	if err != nil {
		return nil, err
	}
	err = client.Login(ctx, infravault.LoginOptions{AppRole: &infravault.AppRoleLogin{
		RoleID: cfg.RoleID, SecretID: cfg.SecretID, SecretIDFile: cfg.SecretIDFile, MountPath: cfg.AppRoleMount,
	}})
	if err != nil {
		return nil, err
	}
	return &namespaceSecrets{client: client, mount: cfg.Mount, path: cfg.Path}, nil
}

// namespaceConfig is the secret of the namespace, laid over the provider's
// configuration block. A namespace without one is unknown: a 404.
func (s *namespaceSecrets) namespaceConfig(ctx context.Context, namespace string) (map[string]any, error) {
	named := strings.NewReplacer("{namespace}", url.PathEscape(namespace))
	config, err := s.client.GetSecret(ctx, named.Replace(s.mount), named.Replace(s.path))
	var e *infraerrors.Error
	if infraerrors.As(err, &e) && e.Code == http.StatusNotFound {
		return nil, infraerrors.NotFound("unknown namespace %q", namespace)
	}
	return config, err
}
