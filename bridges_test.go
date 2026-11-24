package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infrapi/lib/pkg/auth"
	"github.com/infrapi/lib/pkg/config"
	infraerrors "github.com/infrapi/lib/pkg/errors"
	"github.com/infrapi/lib/pkg/mcp"
	"github.com/infrapi/lib/pkg/server"
	"github.com/infrapi/lib/pkg/tofu"
)

// grafanaMCP is an MCP server of read-only tools in families, as
// mcp-grafana's, keeping the headers of each call.
type grafanaMCP struct {
	mu      sync.Mutex
	headers []http.Header
}

func (g *grafanaMCP) last(t *testing.T) http.Header {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	require.NotEmpty(t, g.headers)
	return g.headers[len(g.headers)-1]
}

func (g *grafanaMCP) start(t *testing.T) string {
	t.Helper()
	s := sdk.NewServer(&sdk.Implementation{Name: "mcp-grafana", Version: "v0"}, nil)
	for _, name := range []string{"query_prometheus", "query_loki_logs", "search_dashboards"} {
		s.AddTool(&sdk.Tool{Name: name, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"expr": map[string]any{"type": "string"}}}},
			func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				g.mu.Lock()
				g.headers = append(g.headers, req.Extra.Header.Clone())
				g.mu.Unlock()
				return &sdk.CallToolResult{StructuredContent: map[string]any{"tool": req.Params.Name}}, nil
			})
	}
	srv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

func bridgeFile(url string) configFile {
	return configFile{
		API:      apiConfig{BasePath: "/grafana/v1/namespaces/{namespace}", NamespacePattern: defaultNamespacePattern},
		Provider: providerConfig{Name: "grafana"},
		MCP: &mcpConfig{Bridges: []bridgeConfig{{
			Name: "grafana", URL: url,
			Tools: []bridgeRule{
				{Match: "query_prometheus", Resource: "mcp-prometheus"},
				{Match: "query_loki.*", Resource: "mcp-loki"},
			},
			Headers:     map[string]string{"Authorization": "Bearer ${MCP_TOKEN}"},
			CallHeaders: map[string]string{"X-Grafana-URL": "url", "X-Grafana-Service-Account-Token": "auth"},
		}}},
	}
}

// bridgedEngine is the service as serve sets it up with a bridge: the
// contract with the bridged routes, enforced; the RBAC per route; the MCP
// endpoint with the bridged tools.
func bridgedEngine(t *testing.T, f configFile, rbac accessChecker, configs *namespaceConfigs) string {
	t.Helper()
	t.Setenv("INFRAPI_CONFIG_DOTENV_FILE", "packaging/example.env")
	vars, err := config.NewConfig()
	require.NoError(t, err)
	cfg, err := vars.GetAppConfig()
	require.NoError(t, err)

	bs, err := startBridges(context.Background(), f, configs, func(name string) (string, bool) {
		return map[string]string{"MCP_TOKEN": "server-token"}[name], name == "MCP_TOKEN"
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.close()) })

	a := testAPI(t)
	contract, err := json.Marshal(a.document())
	require.NoError(t, err)
	contract, err = bs.contract(contract)
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	e, err := server.NewServerGin(cfg, []string{"/grafana/v1"},
		server.WithOpenAPI(server.OpenAPISpec{Name: "grafana v1", Path: "grafana/v1.json", Content: contract}))
	require.NoError(t, err)
	h := newHandlers(a, newShared(newFakeProvider(), 10), time.Minute)
	if rbac != nil {
		h.access = &access{checker: rbac, api: "grafana.infrapi.io"}
	}
	h.routes(e)
	require.NoError(t, bs.routes(e, h))
	opts, err := mcpOptions(f, a, bs)
	require.NoError(t, err)
	mh, err := mcp.New(contract, e, opts)
	require.NoError(t, err)
	e.Any("/grafana/v1/-/mcp", gin.WrapH(mh))
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv.URL
}

func namespaceSecretsOf(secrets map[string]map[string]any) *namespaceConfigs {
	return &namespaceConfigs{
		base: map[string]any{"url": "https://grafana.example.com"},
		secret: func(_ context.Context, ns string) (map[string]any, error) {
			s, ok := secrets[ns]
			if !ok {
				return nil, infraerrors.NotFound("unknown namespace %q", ns)
			}
			return s, nil
		},
		ttl: time.Minute, now: time.Now,
	}
}

// A bridged tool is a route of the API: authorized on its resource, called
// with the namespace's own provider configuration and the static headers.
func TestBridgeREST(t *testing.T) {
	g := &grafanaMCP{}
	rbac := &fakeRBAC{allow: func(r auth.AuthorizeRequest) bool { return r.Resource == "mcp-prometheus" }}
	base := bridgedEngine(t, bridgeFile(g.start(t)), rbac, namespaceSecretsOf(map[string]map[string]any{
		"acme": {"auth": "sa-acme"},
		"bare": {},
	}))
	call := func(ns, resource, tool string) (int, map[string]any) {
		req, err := http.NewRequest(http.MethodGet, base+"/grafana/v1/namespaces/"+ns+"/"+resource+"/"+tool+"?"+
			url.Values{"arguments": {`{"expr":"up"}`}}.Encode(), nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		var body map[string]any
		if strings.Contains(resp.Header.Get("Content-Type"), "json") { // gin's own 404 is text
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		}
		return resp.StatusCode, body
	}

	code, body := call("acme", "mcp-prometheus", "query_prometheus")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, map[string]any{"tool": "query_prometheus"}, body)
	h := g.last(t)
	assert.Equal(t, "sa-acme", h.Get("X-Grafana-Service-Account-Token"), "the namespace's secret")
	assert.Equal(t, "https://grafana.example.com", h.Get("X-Grafana-URL"), "provider.config")
	assert.Equal(t, "Bearer server-token", h.Get("Authorization"))
	assert.Equal(t, auth.AuthorizeRequest{Namespace: "acme", Api: "grafana.infrapi.io", Resource: "mcp-prometheus", Verb: "get"}, rbac.asked[len(rbac.asked)-1])

	// the role grants prometheus, not loki
	code, body = call("acme", "mcp-loki", "query_loki_logs")
	assert.Equal(t, http.StatusForbidden, code, body)

	// a namespace without its credential, an unknown one
	code, body = call("bare", "mcp-prometheus", "query_prometheus")
	assert.Equal(t, http.StatusBadGateway, code, body)
	assert.Equal(t, "upstream_auth", body["code"])
	code, _ = call("nobody", "mcp-prometheus", "query_prometheus")
	assert.Equal(t, http.StatusNotFound, code)

	// search_dashboards no rule selects
	code, _ = call("acme", "mcp-dashboards", "search_dashboards")
	assert.Equal(t, http.StatusNotFound, code)
}

// The agent sees the bridged tools beside the provider's, each taking a
// namespace, and is told what they are.
func TestBridgeMCP(t *testing.T) {
	g := &grafanaMCP{}
	base := bridgedEngine(t, bridgeFile(g.start(t)), nil, namespaceSecretsOf(map[string]map[string]any{"acme": {"auth": "sa-acme"}}))
	s := mcpSession(t, base+"/grafana/v1/-/mcp")
	assert.Equal(t, []string{"get_folder", "plan_create_folder", "plan_update_folder", "query_loki_logs", "query_prometheus"}, mcpTools(t, s))
	assert.Contains(t, s.InitializeResult().Instructions, "Tools of grafana (mcp-loki, mcp-prometheus)")

	res, err := s.CallTool(context.Background(), &sdk.CallToolParams{Name: "query_loki_logs", Arguments: map[string]any{"namespace": "acme", "expr": "{job=\"x\"}"}})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)
	assert.Equal(t, "sa-acme", g.last(t).Get("X-Grafana-Service-Account-Token"))
}

// A namespace's configuration is read once per TTL.
func TestNamespaceConfigsCache(t *testing.T) {
	reads := 0
	now := time.Unix(0, 0)
	n := &namespaceConfigs{
		base: map[string]any{"url": "u", "auth": "base"},
		secret: func(context.Context, string) (map[string]any, error) {
			reads++
			if reads == 3 {
				return nil, errors.New("vault down")
			}
			return map[string]any{"auth": "secret"}, nil
		},
		ttl: time.Minute, now: func() time.Time { return now },
	}
	for range 2 {
		c, err := n.of(context.Background(), "acme")
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"url": "u", "auth": "secret"}, c, "the secret over provider.config")
	}
	assert.Equal(t, 1, reads)
	now = now.Add(2 * time.Minute)
	_, err := n.of(context.Background(), "acme")
	require.NoError(t, err)
	assert.Equal(t, 2, reads)
	now = now.Add(2 * time.Minute)
	_, err = n.of(context.Background(), "acme")
	require.ErrorContains(t, err, "vault down")
	assert.Equal(t, map[string]any{"url": "u", "auth": "base"}, n.base, "provider.config untouched")

	shared := &namespaceConfigs{base: map[string]any{"auth": "one"}}
	c, err := shared.of(context.Background(), "any")
	require.NoError(t, err)
	assert.Equal(t, "one", c["auth"], "without namespaces: provider.config")
}

func TestBridgeStartFails(t *testing.T) {
	f := bridgeFile("http://127.0.0.1:1/mcp")
	_, err := startBridges(context.Background(), f, &namespaceConfigs{}, func(string) (string, bool) { return "x", true })
	require.ErrorContains(t, err, "mcp.bridges")
	_, err = startBridges(context.Background(), bridgeFile((&grafanaMCP{}).start(t)), &namespaceConfigs{}, func(string) (string, bool) { return "", false })
	require.ErrorContains(t, err, "MCP_TOKEN")
	bs, err := startBridges(context.Background(), configFile{}, &namespaceConfigs{}, nil)
	require.NoError(t, err)
	assert.Empty(t, bs.tools())
	assert.Empty(t, bs.describe(configFile{}))
}

func TestParseConfigBridges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grafana.yaml")
	write := func(bridges string) {
		require.NoError(t, os.WriteFile(path, []byte("provider:\n  binary_path: ./terraform-provider-grafana_v4.35.0\n"+
			"resources:\n  grafana_folder: {}\nmcp:\n  bridges:\n"+bridges), 0o600))
	}
	write("    - name: grafana\n      url: http://127.0.0.1:8000/mcp\n      tools:\n        - {match: query_prometheus, resource: mcp-prometheus}\n" +
		"      call_headers: {X-Grafana-Service-Account-Token: auth}\n      timeout: 1m\n")
	f, err := readConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "mcp-prometheus", f.MCP.Bridges[0].Tools[0].Resource)
	assert.Equal(t, time.Minute, f.MCP.Bridges[0].Timeout)

	for name, src := range map[string]string{
		"no url":          "    - name: grafana\n      tools: [{match: x, resource: mcp-y}]\n",
		"no tools":        "    - name: grafana\n      url: http://x\n",
		"bad name":        "    - name: Grafana MCP\n      url: http://x\n      tools: [{match: x, resource: mcp-y}]\n",
		"twice":           "    - {name: g, url: http://x, tools: [{match: x, resource: mcp-y}]}\n    - {name: g, url: http://x, tools: [{match: x, resource: mcp-y}]}\n",
		"no mcp- prefix":  "    - name: grafana\n      url: http://x\n      tools: [{match: x, resource: prometheus}]\n",
		"empty key":       "    - name: grafana\n      url: http://x\n      tools: [{match: x, resource: mcp-y}]\n      call_headers: {X-A: \"\"}\n",
		"negative":        "    - name: grafana\n      url: http://x\n      tools: [{match: x, resource: mcp-y}]\n      timeout: -1s\n",
		"unknown field":   "    - name: grafana\n      url: http://x\n      tools: [{match: x, resource: mcp-y}]\n      write: true\n",
		"unknown in rule": "    - name: grafana\n      url: http://x\n      tools: [{match: x, resource: mcp-y, verb: get}]\n",
	} {
		write(src)
		_, err := readConfig(path)
		assert.Error(t, err, name)
	}
}

// The keys only bridges read, which the provider's configuration block does
// not have, are not passed to the provider; any other key is.
func TestProviderOnly(t *testing.T) {
	schema := tofu.Block{
		Attributes: map[string]tofu.Attribute{"url": {}, "auth": {}, "org_id": {}},
		Blocks:     map[string]tofu.NestedBlock{"retry": {}},
	}
	config := map[string]any{"url": "u", "auth": "t", "retry": map[string]any{}, "grafana_org_id": 2, "typo": "x"}
	bridgeKeys := map[string]bool{"auth": true, "grafana_org_id": true}
	assert.Equal(t, map[string]any{"url": "u", "auth": "t", "retry": map[string]any{}, "typo": "x"}, providerOnly(config, schema, bridgeKeys),
		"auth is the provider's too; typo no bridge reads, the provider refuses it")

	f := bridgeFile("http://x")
	assert.Equal(t, map[string]bool{"url": true, "auth": true}, f.bridgeKeys())
	assert.Empty(t, configFile{}.bridgeKeys())
}
