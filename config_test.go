package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infraerrors "github.com/infrapi/lib/pkg/errors"
	"github.com/infrapi/lib/pkg/tofu"
)

const testConfig = `
provider:
  binary_path: providers/registry.terraform.io/grafana/grafana/4.35.0/linux_amd64/terraform-provider-grafana_v4.35.0
  config:
    url: http://grafana:3000
    auth: ${GRAFANA_AUTH}
    retry_status_codes: ["429", "${RETRY_ON}"]
    note: costs $$5
types:
  grafana_team: {}
  grafana_folder: {}
`

func TestParseConfig(t *testing.T) {
	f, err := parseConfig([]byte(testConfig), "grafana.yaml")
	require.NoError(t, err)
	assert.Equal(t, "grafana", f.Provider.Name, "from the binary's name")
	assert.Equal(t, []string{"grafana_folder", "grafana_team"}, f.typeNames())
	assert.Equal(t, "${GRAFANA_AUTH}", f.Provider.Config["auth"], "the environment is read only to serve")
	assert.Equal(t, 10, f.Provider.Parallelism)
	assert.Equal(t, 10*time.Minute, f.Provider.Timeout)

	env := map[string]string{"GRAFANA_AUTH": "admin:s3cret", "RETRY_ON": "5xx"}
	config, err := f.Provider.resolved(func(name string) (string, bool) { v, ok := env[name]; return v, ok })
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"url":                "http://grafana:3000",
		"auth":               "admin:s3cret",
		"retry_status_codes": []any{"429", "5xx"},
		"note":               "costs $5",
	}, config)

	_, err = f.Provider.resolved(func(string) (string, bool) { return "", false })
	assert.ErrorContains(t, err, "GRAFANA_AUTH")
}

func TestParseConfigSettings(t *testing.T) {
	f, err := parseConfig([]byte("provider:\n  binary_path: ./terraform-provider-grafana_v4.35.0\n  name: dashboards\n  parallelism: 2\n  timeout: 90s\ntypes:\n  grafana_folder: {}\n"), "grafana.yaml")
	require.NoError(t, err)
	assert.Equal(t, "dashboards", f.Provider.Name)
	assert.Equal(t, 2, f.Provider.Parallelism)
	assert.Equal(t, 90*time.Second, f.Provider.Timeout)
}

func TestParseConfigRejects(t *testing.T) {
	for name, src := range map[string]string{
		"empty":          "",
		"no binary_path": "types:\n  grafana_folder: {}\n",
		"no types":       "provider:\n  binary_path: ./terraform-provider-grafana\n",
		"unknown key":    "provider:\n  binary_path: ./terraform-provider-grafana\n  path: x\ntypes:\n  grafana_folder: {}\n",
		"no parallelism": "provider:\n  binary_path: ./terraform-provider-grafana\n  parallelism: -1\ntypes:\n  grafana_folder: {}\n",
		"not a duration": "provider:\n  binary_path: ./terraform-provider-grafana\n  timeout: soon\ntypes:\n  grafana_folder: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig([]byte(src), "grafana.yaml")
			assert.Error(t, err)
		})
	}
}

type fakeSchemas map[string]tofu.Block

func (f fakeSchemas) ResourceTypes() []string {
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	return names
}

func (f fakeSchemas) ResourceSchema(resourceType string) (tofu.Block, error) {
	b, ok := f[resourceType]
	if !ok {
		return tofu.Block{}, infraerrors.NotFound("no resource type %q", resourceType)
	}
	return b, nil
}

func TestLoadSchemasUnknownType(t *testing.T) {
	_, err := loadSchemas(fakeSchemas{"grafana_folder": folder}, []string{"grafana_foldr"})
	assert.ErrorContains(t, err, `provider has no resource type "grafana_foldr" (it has 1, e.g. [grafana_folder])`)
}

func TestParseConfigNamespaces(t *testing.T) {
	f, err := parseConfig([]byte(`
provider:
  binary_path: ./terraform-provider-grafana
namespaces:
  vault:
    address: ${VAULT_ADDR}
    role_id: ${VAULT_ROLE_ID}
    secret_id_file: /run/secrets/vault-secret-id
    path: tofu-api/grafana/{namespace}
types:
  grafana_folder: {}
`), "grafana.yaml")
	require.NoError(t, err)
	require.NotNil(t, f.Namespaces)
	assert.Equal(t, namespacesConfig{Idle: 10 * time.Minute, MaxAge: time.Hour, MaxProviders: 20, Vault: vaultConfig{
		Address: "${VAULT_ADDR}", AppRoleMount: "approle", RoleID: "${VAULT_ROLE_ID}",
		SecretIDFile: "/run/secrets/vault-secret-id", Mount: "secret", Path: "tofu-api/grafana/{namespace}",
	}}, *f.Namespaces)

	env := map[string]string{"VAULT_ADDR": "https://vault:8200", "VAULT_ROLE_ID": "role"}
	v, err := f.Namespaces.Vault.resolved(func(name string) (string, bool) { s, ok := env[name]; return s, ok })
	require.NoError(t, err)
	assert.Equal(t, "https://vault:8200", v.Address)
	assert.Equal(t, "role", v.RoleID)

	_, err = f.Namespaces.Vault.resolved(func(string) (string, bool) { return "", false })
	assert.ErrorContains(t, err, "namespaces.vault.address: environment variable VAULT_ADDR is not set")
}

func TestParseConfigNamespacesRejects(t *testing.T) {
	const head = "provider:\n  binary_path: ./terraform-provider-grafana\ntypes:\n  grafana_folder: {}\nnamespaces:\n"
	for name, src := range map[string]string{
		"no vault":      "  idle: 5m\n",
		"no namespace":  "  vault: {address: http://vault, role_id: r, secret_id: s, mount: kv, path: grafana}\n",
		"no secret":     "  vault: {address: http://vault, role_id: r, path: 'x/{namespace}'}\n",
		"two secrets":   "  vault: {address: http://vault, role_id: r, secret_id: s, secret_id_file: f, path: 'x/{namespace}'}\n",
		"negative idle": "  idle: -1m\n  vault: {address: http://vault, role_id: r, secret_id: s, path: 'x/{namespace}'}\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig([]byte(head+src), "grafana.yaml")
			assert.Error(t, err)
		})
	}
}

func TestParseConfigBasePath(t *testing.T) {
	src := func(basePath string) []byte {
		return fmt.Appendf(nil, "api:\n  base_path: %q\nprovider:\n  binary_path: ./terraform-provider-grafana_v4.35.0\ntypes:\n  grafana_folder: {}\n", basePath)
	}
	for basePath, want := range map[string]string{
		"":                        "/grafana/v1/namespaces/{namespace}",
		"/{provider}/{namespace}": "/grafana/{namespace}",
		"/infra/x-{provider}/v2/{namespace}/stacks": "/infra/x-grafana/v2/{namespace}/stacks",
	} {
		f, err := parseConfig(src(basePath), "grafana.yaml")
		require.NoError(t, err, basePath)
		assert.Equal(t, want, f.API.BasePath)
	}
	for name, basePath := range map[string]string{
		"not absolute":    "grafana/{namespace}",
		"final slash":     "/grafana/{namespace}/",
		"not clean":       "/grafana//{namespace}",
		"root":            "/",
		"no namespace":    "/grafana/v1",
		"twice":           "/{namespace}/{namespace}",
		"part of segment": "/orgs/ns-{namespace}",
		"other parameter": "/{region}/{namespace}",
		"gin parameter":   "/:region/{namespace}",
		"operational":     "/-/{namespace}",
	} {
		_, err := parseConfig(src(basePath), "grafana.yaml")
		assert.ErrorContains(t, err, "api.base_path", name)
	}
}

func TestParseConfigNamespacePattern(t *testing.T) {
	src := func(pattern string) []byte {
		return fmt.Appendf(nil, "api:\n  namespace_pattern: %q\nprovider:\n  binary_path: ./terraform-provider-grafana_v4.35.0\ntypes:\n  grafana_folder: {}\n", pattern)
	}
	for pattern, want := range map[string]string{
		"":                defaultNamespacePattern,
		`^[A-Za-z0-9.]+$`: `^[A-Za-z0-9.]+$`,
	} {
		f, err := parseConfig(src(pattern), "grafana.yaml")
		require.NoError(t, err, pattern)
		assert.Equal(t, want, f.API.NamespacePattern)
	}
	_, err := parseConfig(src(`^[a-z`), "grafana.yaml")
	assert.ErrorContains(t, err, "api.namespace_pattern")

	re := regexp.MustCompile(defaultNamespacePattern)
	for _, ns := range []string{"core", "testv4_sandbox", "team-a", "0"} {
		assert.True(t, re.MatchString(ns), ns)
	}
	for _, ns := range []string{"", "Core", "-a", "_a", "a/b", "a.b", strings.Repeat("a", 64)} {
		assert.False(t, re.MatchString(ns), ns)
	}
}
