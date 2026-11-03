package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infraerrors "github.com/infrapi/lib/pkg/errors"
)

// fakeVault answers AppRole logins and KV v2 reads, as Vault's API does;
// pkg/vault keeps the token valid, and has its own tests for it.
type fakeVault struct {
	mu      sync.Mutex
	logins  int
	valid   map[string]bool // tokens Vault accepts
	secrets map[string]map[string]any
	denied  map[string]bool // paths the AppRole's policy does not cover
}

func newFakeVault(t *testing.T) (*fakeVault, *httptest.Server) {
	f := &fakeVault{valid: map[string]bool{}, secrets: map[string]map[string]any{
		"/v1/secret/data/tofu-api/grafana/acme": {"auth": "glsa_acme"},
		"/v1/kv-acme/data/grafana":              {"auth": "glsa_kv_acme"},
	}, denied: map[string]bool{"/v1/kv-denied/data/grafana": true}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
	if r.URL.Path == "/v1/auth/approle/login" {
		var in struct {
			RoleID   string `json:"role_id"`
			SecretID string `json:"secret_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.RoleID != "role" || in.SecretID != "s3cret" {
			w.WriteHeader(http.StatusBadRequest)
			reply(map[string]any{"errors": []string{"invalid role or secret ID"}})
			return
		}
		f.logins++
		token := "token-" + strings.Repeat("x", f.logins)
		f.valid[token] = true
		// Vault answers with "data": null beside "auth".
		reply(map[string]any{"data": nil, "auth": map[string]any{"client_token": token, "lease_duration": 1200}})
		return
	}
	if !f.valid[r.Header.Get("X-Vault-Token")] || f.denied[r.URL.Path] {
		w.WriteHeader(http.StatusForbidden)
		reply(map[string]any{"errors": []string{"permission denied"}})
		return
	}
	secret, ok := f.secrets[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		reply(map[string]any{"errors": []string{}})
		return
	}
	reply(map[string]any{"data": map[string]any{"data": secret, "metadata": map[string]any{"version": 1}}})
}

func testSecrets(t *testing.T, addr string, cfg vaultConfig) (*namespaceSecrets, error) {
	t.Helper()
	cfg.Address, cfg.AppRoleMount, cfg.RoleID, cfg.Mount = addr, "approle", "role", "secret"
	cfg.Path = "tofu-api/grafana/{namespace}"
	return newNamespaceSecrets(context.Background(), cfg, -1)
}

func TestNamespaceSecrets(t *testing.T) {
	f, srv := newFakeVault(t)
	s, err := testSecrets(t, srv.URL, vaultConfig{SecretID: "s3cret"})
	require.NoError(t, err)

	config, err := s.namespaceConfig(context.Background(), "acme")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"auth": "glsa_acme"}, config, "the values of the KV v2 secret")
	assert.Equal(t, 1, f.logins)

	_, err = s.namespaceConfig(context.Background(), "beta")
	var e *infraerrors.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, http.StatusNotFound, e.Code, "a namespace without a secret is unknown")
	assert.Equal(t, `unknown namespace "beta"`, e.Message)
}

// An engine per namespace: the mount names it, the path may not.
func TestNamespaceSecretsMountPerNamespace(t *testing.T) {
	_, srv := newFakeVault(t)
	s, err := newNamespaceSecrets(context.Background(), vaultConfig{
		Address: srv.URL, AppRoleMount: "approle", RoleID: "role", SecretID: "s3cret",
		Mount: "kv-{namespace}", Path: "grafana",
	}, -1)
	require.NoError(t, err)

	config, err := s.namespaceConfig(context.Background(), "acme")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"auth": "glsa_kv_acme"}, config)

	// No engine, as Vault answers when the policy covers its path.
	_, err = s.namespaceConfig(context.Background(), "beta")
	var e *infraerrors.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, http.StatusNotFound, e.Code)

	// The policy does not cover it: the service's own mistake, not an
	// unknown namespace.
	_, err = s.namespaceConfig(context.Background(), "denied")
	require.ErrorAs(t, err, &e)
	assert.Equal(t, "upstream_auth", e.Slug)
}

func TestNamespaceSecretsSecretIDFile(t *testing.T) {
	_, srv := newFakeVault(t)
	file := filepath.Join(t.TempDir(), "secret-id")
	require.NoError(t, os.WriteFile(file, []byte("s3cret\n"), 0o600))
	_, err := testSecrets(t, srv.URL, vaultConfig{SecretIDFile: file})
	require.NoError(t, err)
}

// A login refused stops the service at startup.
func TestNamespaceSecretsLoginRefused(t *testing.T) {
	_, srv := newFakeVault(t)
	_, err := testSecrets(t, srv.URL, vaultConfig{SecretID: "wrong"})
	var e *infraerrors.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, "upstream_auth", e.Slug)
}

func TestNamespaceSecretsUnavailable(t *testing.T) {
	_, srv := newFakeVault(t)
	s, err := testSecrets(t, srv.URL, vaultConfig{SecretID: "s3cret"})
	require.NoError(t, err)
	srv.Close()
	_, err = s.namespaceConfig(context.Background(), "acme")
	var e *infraerrors.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, http.StatusServiceUnavailable, e.Code)
}
