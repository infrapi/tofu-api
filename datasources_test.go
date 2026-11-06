package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	"github.com/infrapi/lib/pkg/auth"
	"github.com/infrapi/lib/pkg/tofu"
)

// folders looks like grafana_folders: arguments of every kind the query
// carries, one it does not, one the service fixes, and what it reads.
var folders = tofu.Block{
	Description: "Folders.",
	Attributes: map[string]tofu.Attribute{
		"title":  {Usage: tofu.UsageOptional, Type: cty.String, Description: "A title."},
		"tags":   {Usage: tofu.UsageOptional, Type: cty.List(cty.String)},
		"limit":  {Usage: tofu.UsageOptional, Type: cty.Number},
		"nested": {Usage: tofu.UsageOptional, Type: cty.Bool},
		"filter": {Usage: tofu.UsageOptional, Type: cty.Object(map[string]cty.Type{"q": cty.String})},
		"org_id": {Usage: tofu.UsageOptional, Type: cty.String},
		"id":     {Usage: tofu.UsageComputed, Type: cty.String},
		"titles": {Usage: tofu.UsageComputed, Type: cty.List(cty.String)},
	},
}

const testDataRules = `
data_sources:
  grafana_folders:
    attributes:
      org_id:
        value: ns
        hidden: true
      filter:
        hidden: true
`

// testDataAPI is testAPI with the data source grafana_folders, and the data
// source grafana_folder beside the resource type of the same name.
func testDataAPI(t *testing.T) *api {
	t.Helper()
	a := testAPI(t)
	f, err := parseConfig([]byte("provider:\n  binary_path: terraform-provider-grafana_v4.35.0\n"+testDataRules+"  grafana_folder: {}\n"), "grafana.yaml")
	require.NoError(t, err)
	a.dataSources = map[string]tofu.Block{"grafana_folders": folders, "grafana_folder": {Attributes: map[string]tofu.Attribute{
		"title": {Usage: tofu.UsageRequired, Type: cty.String},
		"uid":   {Usage: tofu.UsageComputed, Type: cty.String},
	}}}
	a.dataRules, err = parseDataRules(f, "grafana.yaml", a.dataSources)
	require.NoError(t, err)
	return a
}

func newDataEngine(t *testing.T, p provider, rbac accessChecker) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	h := newHandlers(testDataAPI(t), newShared(p, 10), time.Minute)
	if rbac != nil {
		h.access = &access{checker: rbac, api: "grafana.example.com"}
	}
	h.routes(e)
	return e
}

// The query becomes the arguments, each to its type, with the rules
// applied; the response is what the data source reads, without what the
// rules hide.
func TestReadDataSource(t *testing.T) {
	p := newFakeProvider()
	e := newDataEngine(t, p, nil)

	code, body := do(t, e, http.MethodGet, "/grafana/v1/namespaces/acme/folders?title=Ops&tags=a&tags=b&limit=3&nested=true", "")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, map[string]any{
		"title": "Ops", "tags": []any{"a", "b"}, "limit": float64(3), "nested": true, "org_id": "acme",
	}, p.read, "typed arguments, and the namespace's value")
	assert.Equal(t, []any{"Ops", "Dev"}, body["titles"])
	assert.NotContains(t, body, "org_id", "hidden")

	code, _ = do(t, e, http.MethodGet, "/grafana/v1/namespaces/acme/folders", "")
	assert.Equal(t, http.StatusOK, code, "no argument is required")
}

func TestReadDataSourceRefusesTheQuery(t *testing.T) {
	e := newDataEngine(t, newFakeProvider(), nil)
	for name, query := range map[string]string{
		"not an argument": "?colour=red",
		"fixed by a rule": "?org_id=beta",
		"hidden":          "?filter=x",
		"computed":        "?titles=x",
		"not a number":    "?limit=many",
		"not a boolean":   "?nested=maybe",
		"twice":           "?title=a&title=b",
	} {
		code, _ := do(t, e, http.MethodGet, "/grafana/v1/namespaces/acme/folders"+query, "")
		assert.Equal(t, http.StatusBadRequest, code, name)
	}
}

// A data source and a resource type of the same name share their path: GET
// reads the data source, POST creates.
func TestDataSourceBesideResourceType(t *testing.T) {
	p := newFakeProvider()
	e := newDataEngine(t, p, nil)

	code, _ := do(t, e, http.MethodPost, "/grafana/v1/namespaces/acme/folder", `{"title":"Ops"}`)
	assert.Equal(t, http.StatusCreated, code)
	code, _ = do(t, e, http.MethodGet, "/grafana/v1/namespaces/acme/folder?title=Ops", "")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, map[string]any{"title": "Ops"}, p.read)
}

// Reading a data source is listing its resource for the RBAC.
func TestReadDataSourceAsksTheRBAC(t *testing.T) {
	rbac := &fakeRBAC{allow: func(auth.AuthorizeRequest) bool { return true }}
	e := newDataEngine(t, newFakeProvider(), rbac)

	code, _ := doAs(t, e, "tok", http.MethodGet, "/grafana/v1/namespaces/acme/folders", "")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, rbac.asked, 1)
	assert.Equal(t, auth.AuthorizeRequest{Namespace: "acme", Api: "grafana.example.com", Resource: "folders", Verb: "list"}, rbac.asked[0])
}

// The contract has a GET per data source, its arguments in the query, and
// stays valid OpenAPI with a data source beside a resource type.
func TestDataSourceContract(t *testing.T) {
	a := testDataAPI(t)
	raw, err := json.Marshal(a.document())
	require.NoError(t, err)
	doc, err := openapi3.NewLoader().LoadFromData(raw)
	require.NoError(t, err)
	require.NoError(t, doc.Validate(context.Background()))

	get := doc.Paths.Find("/grafana/v1/namespaces/{namespace}/folders").Get
	require.NotNil(t, get)
	assert.Equal(t, "readFolders", get.OperationID)
	var names []string
	for _, p := range get.Parameters {
		names = append(names, p.Value.Name)
	}
	assert.Equal(t, []string{"limit", "nested", "tags", "title"}, names, "neither fixed, hidden nor computed")
	tags := get.Parameters.GetByInAndName("query", "tags")
	assert.Equal(t, "form", tags.Style)
	assert.True(t, *tags.Explode)
	assert.Contains(t, doc.Components.Schemas["FoldersData"].Value.Properties, "titles")
	assert.NotContains(t, doc.Components.Schemas["FoldersData"].Value.Properties, "org_id")

	folder := doc.Paths.Find("/grafana/v1/namespaces/{namespace}/folder")
	require.NotNil(t, folder.Get, "the data source")
	require.NotNil(t, folder.Post, "the resource type")
	assert.True(t, folder.Get.Parameters.GetByInAndName("query", "title").Required)
}

func TestParseDataRulesRefuses(t *testing.T) {
	sources := map[string]tofu.Block{"grafana_folders": folders}
	for name, src := range map[string]string{
		"object in the query": "data_sources:\n  grafana_folders: {}\n",
		"computed":            "data_sources:\n  grafana_folders:\n    attributes:\n      filter: {hidden: true}\n      titles: {hidden: true}\n",
		"unknown":             "data_sources:\n  grafana_folders:\n    attributes:\n      filter: {hidden: true}\n      colour: {hidden: true}\n",
	} {
		f, err := parseConfig([]byte("provider:\n  binary_path: terraform-provider-grafana_v4.35.0\n"+src), "grafana.yaml")
		require.NoError(t, err, name)
		_, err = parseDataRules(f, "grafana.yaml", sources)
		assert.ErrorContains(t, err, "data_sources.grafana_folders.attributes.", name)
	}

	f, err := parseConfig([]byte("provider:\n  binary_path: terraform-provider-grafana_v4.35.0\ndata_sources:\n  grafana_folders:\n    attributes:\n      filter: {default: '{\"q\": ns}'}\n"), "grafana.yaml")
	require.NoError(t, err, "data sources alone")
	_, err = parseDataRules(f, "grafana.yaml", sources)
	assert.NoError(t, err, "an object argument the service sets")
}
