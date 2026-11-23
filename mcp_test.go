package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infrapi/lib/pkg/config"
	"github.com/infrapi/lib/pkg/mcp"
	"github.com/infrapi/lib/pkg/server"
)

// mcpEngine is the service as serve sets it up with an mcp section: the
// lib's server, the routes, the MCP endpoint under the API's root.
func mcpEngine(t *testing.T, p provider, m *mcpConfig) string {
	t.Helper()
	t.Setenv("INFRAPI_CONFIG_DOTENV_FILE", "packaging/example.env")
	vars, err := config.NewConfig()
	require.NoError(t, err)
	cfg, err := vars.GetAppConfig()
	require.NoError(t, err)

	a := testAPI(t)
	contract, err := json.Marshal(a.document())
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	e, err := server.NewServerGin(cfg, []string{"/grafana/v1"},
		server.WithOpenAPI(server.OpenAPISpec{Name: "grafana v1", Path: "grafana/v1.json", Content: contract}))
	require.NoError(t, err)
	newHandlers(a, newShared(p, 10), time.Minute).routes(e)

	f := configFile{Provider: providerConfig{Name: "grafana"}, MCP: m}
	require.True(t, m.enabled())
	opts, err := mcpOptions(f, a)
	require.NoError(t, err)
	mh, err := mcp.New(contract, e, opts)
	require.NoError(t, err)
	e.Any("/grafana/v1/-/mcp", gin.WrapH(mh))

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv.URL + "/grafana/v1/-/mcp"
}

func mcpSession(t *testing.T, endpoint string) *sdk.ClientSession {
	t.Helper()
	s, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil).
		Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: endpoint}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close()) })
	return s
}

func mcpTools(t *testing.T, s *sdk.ClientSession) []string {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

// Read only by default: reading a folder, and planning its create and
// update; the writes with writes: true.
func TestMCPTools(t *testing.T) {
	s := mcpSession(t, mcpEngine(t, newFakeProvider(), &mcpConfig{}))
	assert.Equal(t, []string{"get_folder", "plan_create_folder", "plan_update_folder"}, mcpTools(t, s))

	s = mcpSession(t, mcpEngine(t, newFakeProvider(), &mcpConfig{Writes: true}))
	assert.Equal(t, []string{"create_folder", "delete_folder", "get_folder", "plan_create_folder", "plan_update_folder", "update_folder"}, mcpTools(t, s))

	s = mcpSession(t, mcpEngine(t, newFakeProvider(), nil))
	assert.Equal(t, []string{"get_folder", "plan_create_folder", "plan_update_folder"}, mcpTools(t, s), "served without an mcp section")
}

func TestMCPDisabled(t *testing.T) {
	off := false
	assert.False(t, (&mcpConfig{Enabled: &off}).enabled())
	assert.True(t, (*mcpConfig)(nil).enabled())
	_, err := mcpOptions(configFile{MCP: &mcpConfig{Enabled: &off}}, testAPI(t))
	assert.Error(t, err)
}

// Each resource type has a generated document, from its schema and rules,
// and two prompts; a configured one of the same name replaces it.
func TestMCPGenerated(t *testing.T) {
	s := mcpSession(t, mcpEngine(t, newFakeProvider(), &mcpConfig{
		Prompts: []mcpPrompt{{Name: "plan-create-folder", Text: "Mine."}},
	}))
	ctx := context.Background()
	doc, err := s.ReadResource(ctx, &sdk.ReadResourceParams{URI: "doc://grafana_folder"})
	require.NoError(t, err)
	text := doc.Contents[0].Text
	assert.Contains(t, text, "| `title` | string | you, required |")
	assert.Contains(t, text, "default: `spec.title.replace(\" \", \"-\")`", "the rule's default")
	assert.Contains(t, text, "at most 40 characters")
	assert.Contains(t, text, "| `id` | string | the provider |")
	assert.NotContains(t, text, "org_id", "hidden")
	assert.NotContains(t, text, "`old`", "hidden")
	assert.Contains(t, text, `"title": "example"`, "a minimal body")
	assert.NotContains(t, text, "`create_folder`", "read only: no write tool named")

	p, err := s.GetPrompt(ctx, &sdk.GetPromptParams{Name: "check-drift-folder"})
	require.NoError(t, err)
	assert.Contains(t, p.Messages[0].Content.(*sdk.TextContent).Text, "plan_update_folder")
	p, err = s.GetPrompt(ctx, &sdk.GetPromptParams{Name: "plan-create-folder"})
	require.NoError(t, err)
	assert.Equal(t, "Mine.", p.Messages[0].Content.(*sdk.TextContent).Text, "the configured prompt")

	assert.Contains(t, s.InitializeResult().Instructions, "doc://grafana_folder")
}

// A tool goes through the API: the rules apply, and a plan applies nothing.
func TestMCPCallsTheAPI(t *testing.T) {
	p := newFakeProvider()
	s := mcpSession(t, mcpEngine(t, p, &mcpConfig{Writes: true}))
	ctx := context.Background()

	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "create_folder", Arguments: map[string]any{
		"namespace": "acme", "body": map[string]any{"title": "Ops", "permission": []any{map[string]any{"role": "Viewer"}}},
	}})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)
	id := res.StructuredContent.(map[string]any)["id"].(string)
	assert.Equal(t, "acme", p.objects[id]["org_id"], "the rule's value, from the path's namespace")
	assert.NotContains(t, res.StructuredContent, "org_id", "hidden")

	res, err = s.CallTool(ctx, &sdk.CallToolParams{Name: "get_folder", Arguments: map[string]any{"namespace": "beta", "id": id}})
	require.NoError(t, err)
	assert.True(t, res.IsError, "another namespace's folder is a 404")
}

func TestMCPPlan(t *testing.T) {
	p := newFakeProvider()
	s := mcpSession(t, mcpEngine(t, p, &mcpConfig{}))
	res, err := s.CallTool(context.Background(), &sdk.CallToolParams{Name: "plan_create_folder", Arguments: map[string]any{
		"namespace": "acme", "body": map[string]any{"title": "Ops", "permission": []any{map[string]any{"role": "Viewer"}}},
	}})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)
	assert.Equal(t, "create", res.StructuredContent.(map[string]any)["action"])
	assert.Empty(t, p.objects, "nothing created")
}

// The instructions say how the tools map to the provider, then the
// configured ones; documents and prompts are offered.
func TestMCPContext(t *testing.T) {
	s := mcpSession(t, mcpEngine(t, newFakeProvider(), &mcpConfig{
		Instructions: "A namespace is a team's Grafana organization.",
		Documents:    []mcpDocument{{Name: "folders", Content: "# Folders"}},
		Prompts:      []mcpPrompt{{Name: "check-drift", Text: "Plan the folder."}},
	}))
	instructions := s.InitializeResult().Instructions
	assert.Contains(t, instructions, `Terraform provider "grafana"`)
	assert.Contains(t, instructions, "grafana_folder")
	assert.Contains(t, instructions, "read only")
	assert.Contains(t, instructions, "A namespace is a team's Grafana organization.")
	assert.Contains(t, instructions, "doc://folders")

	doc, err := s.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "doc://folders"})
	require.NoError(t, err)
	assert.Equal(t, "# Folders", doc.Contents[0].Text)
	_, err = s.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: "check-drift"})
	require.NoError(t, err)
}

func TestParseConfigMCP(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "folders.md"), []byte("# Folders"), 0o600))
	path := filepath.Join(dir, "grafana.yaml")
	write := func(mcp string) {
		require.NoError(t, os.WriteFile(path, []byte("provider:\n  binary_path: ./terraform-provider-grafana_v4.35.0\nresources:\n  grafana_folder: {}\nmcp:\n"+mcp), 0o600))
	}

	write("  documents:\n    - name: folders\n      file: folders.md\n")
	f, err := readConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "# Folders", f.MCP.Documents[0].Content, "read relative to the file")

	for name, src := range map[string]string{
		"missing file":  "  documents:\n    - name: folders\n      file: nope.md\n",
		"no content":    "  documents:\n    - name: folders\n",
		"both":          "  prompts:\n    - name: p\n      text: x\n      file: folders.md\n",
		"bad name":      "  prompts:\n    - name: Bad Name\n      text: x\n",
		"twice":         "  prompts:\n    - name: p\n      text: x\n    - name: p\n      text: y\n",
		"unknown field": "  write: true\n",
	} {
		write(src)
		_, err := readConfig(path)
		assert.Error(t, err, name)
	}
}

// The endpoint is under the API's root, beside its other endpoints.
func TestMCPEndpoint(t *testing.T) {
	endpoint := mcpEngine(t, newFakeProvider(), &mcpConfig{})
	resp, err := http.Get(endpoint)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.NotEqual(t, http.StatusNotFound, resp.StatusCode)
}
