package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	"github.com/infrapi/lib/pkg/tofu"
)

// folder looks like grafana_folder, with one of everything the conversion
// treats apart.
var folder = tofu.Block{
	Description: "A folder.",
	Attributes: map[string]tofu.Attribute{
		"id":     {Usage: tofu.UsageOptionalComputed, Type: cty.String},
		"org_id": {Usage: tofu.UsageOptional, Type: cty.String},
		"title":  {Usage: tofu.UsageRequired, Type: cty.String, Description: "The title."},
		"uid":    {Usage: tofu.UsageOptionalComputed, Type: cty.String},
		"url":    {Usage: tofu.UsageComputed, Type: cty.String},
		"token":  {Usage: tofu.UsageOptional, Type: cty.String, Sensitive: true},
		"key_wo": {Usage: tofu.UsageOptional, Type: cty.String, Sensitive: true, WriteOnly: true},
		"old":    {Usage: tofu.UsageOptional, Type: cty.Bool, Deprecated: true},
		"labels": {Usage: tofu.UsageOptional, Type: cty.Map(cty.String)},
		"tags":   {Usage: tofu.UsageOptional, Type: cty.Set(cty.String)},
	},
	Blocks: map[string]tofu.NestedBlock{
		"permission": {
			Nesting:  tofu.NestingList,
			MinItems: 1,
			Block: tofu.Block{Attributes: map[string]tofu.Attribute{
				"role": {Usage: tofu.UsageRequired, Type: cty.String},
			}},
		},
	},
}

const testRules = `
types:
  grafana_folder:
    attributes:
      org_id:
        value: ns
        hidden: true
      old:
        hidden: true
      uid:
        default: spec.title.replace(" ", "-")
        pattern: '^[a-zA-Z0-9_-]{1,40}$'
        maxLength: 40

errors:
  - code: already_exists
    match: '\(status 412\)'
    status: 409
  - code: upstream_unavailable
    match: connection refused
    status: 503
    retryable: true
`

func testAPI(t *testing.T) *api {
	t.Helper()
	types := map[string]tofu.Block{"grafana_folder": folder}
	rs, errs, err := testParseRules(testRules, types)
	require.NoError(t, err)
	return &api{provider: "grafana", basePath: "/grafana/v1/namespaces/{namespace}", namespacePattern: defaultNamespacePattern, types: types, rules: rs, errors: errs}
}

func TestSpecSchema(t *testing.T) {
	a := testAPI(t)
	s := specSchema(folder, a.rules["grafana_folder"], true)
	props := s["properties"].(object)

	for _, name := range []string{"id", "url", "org_id", "old"} {
		assert.NotContains(t, props, name, "computed, id, fixed and hidden attributes are not the caller's")
	}
	assert.Equal(t, []string{"permission", "title"}, s["required"])
	assert.Equal(t, false, s["additionalProperties"])
	assert.Equal(t, object{"type": "string", "format": "password"}, props["token"])
	assert.Equal(t, object{"type": "string", "format": "password", "writeOnly": true}, props["key_wo"])
	assert.Equal(t, true, specSchema(folder, nil, true)["properties"].(object)["old"].(object)["deprecated"])
	assert.Equal(t, object{
		"type":        "string",
		"pattern":     "^[a-zA-Z0-9_-]{1,40}$",
		"maxLength":   40,
		"x-default":   `spec.title.replace(" ", "-")`,
		"description": "Defaults to `spec.title.replace(\" \", \"-\")`.",
	}, props["uid"])
	assert.Equal(t, object{"type": "array", "items": object{"type": "string"}, "uniqueItems": true}, props["tags"])
	assert.Equal(t, object{"type": "object", "additionalProperties": object{"type": "string"}}, props["labels"])

	perm := props["permission"].(object)
	assert.Equal(t, "array", perm["type"])
	assert.Equal(t, int64(1), perm["minItems"])
	assert.Equal(t, []string{"role"}, perm["items"].(object)["required"])
}

func TestStateSchema(t *testing.T) {
	props := stateSchema(folder, testAPI(t).rules["grafana_folder"])["properties"].(object)
	assert.Contains(t, props, "url")
	assert.Contains(t, props, "id")
	assert.Equal(t, object{"type": "string", "format": "password"}, props["token"], "sensitive, returned")
	assert.NotContains(t, props, "key_wo", "write-only: the provider never reports it")
	assert.NotContains(t, props, "org_id", "hidden")
	assert.NotContains(t, props, "old", "hidden")

	props = stateSchema(folder, nil)["properties"].(object)
	assert.Contains(t, props, "org_id", "shown without a rule")
}

func TestTypeSchema(t *testing.T) {
	assert.Equal(t, object{}, typeSchema(cty.DynamicPseudoType))
	assert.Equal(t, object{"type": "array", "minItems": 2, "maxItems": 2}, typeSchema(cty.Tuple([]cty.Type{cty.String, cty.Number})))
	assert.Equal(t, object{
		"type":                 "object",
		"properties":           object{"n": object{"type": "number"}},
		"additionalProperties": false,
	}, typeSchema(cty.Object(map[string]cty.Type{"n": cty.Number})))
}

// The server refuses to start on a contract kin-openapi rejects.
func TestDocumentIsValidOpenAPI(t *testing.T) {
	bearer := testAPI(t)
	bearer.bearer = true
	raw, err := json.Marshal(bearer.document())
	require.NoError(t, err)
	withAuth, err := openapi3.NewLoader().LoadFromData(raw)
	require.NoError(t, err)
	require.NoError(t, withAuth.Validate(context.Background()), "with auth")

	raw, err = json.Marshal(testAPI(t).document())
	require.NoError(t, err)
	doc, err := openapi3.NewLoader().LoadFromData(raw)
	require.NoError(t, err)
	require.NoError(t, doc.Validate(context.Background()))

	assert.NotNil(t, doc.Paths.Find("/grafana/v1/namespaces/{namespace}/folder/{id}").Put)
	assert.NotNil(t, doc.Components.Schemas["FolderSpec"])
}

// testParseRules parses rules as a configuration file holding them would.
func testParseRules(src string, types map[string]tofu.Block) (rules, []errorRule, error) {
	f, err := parseConfig([]byte("provider:\n  binary_path: terraform-provider-grafana_v4.35.0\n"+src), "grafana.yaml")
	if err != nil {
		return nil, nil, err
	}
	return parseRules(f, "grafana.yaml", types)
}

func TestCamel(t *testing.T) {
	assert.Equal(t, "FolderPermissionItem", camel("folder_permission_item"))
	assert.Equal(t, "Pet", camel("pet"))
}

func TestParseRulesRejects(t *testing.T) {
	types := map[string]tofu.Block{"grafana_folder": folder}
	attr := func(name, body string) string {
		return "types:\n  grafana_folder:\n    attributes:\n      " + name + ":\n" + body
	}
	for name, src := range map[string]string{
		"not yaml":            "types: [",
		"unknown key":         "type: {}",
		"unknown attribute":   attr("nope", "        value: ns\n"),
		"computed attribute":  attr("url", "        value: ns\n"),
		"value reads spec":    attr("org_id", "        value: spec.title\n"),
		"default reads state": attr("uid", "        default: spec.url\n"),
		"default reads spec":  attr("uid", "        default: string(spec)\n"),
		"unknown variable":    attr("uid", "        default: region\n"),
		"syntax error":        attr("uid", "        default: spec.title.replace(\n"),
		"both":                attr("uid", "        value: ns\n        default: ns\n"),
		"bad pattern":         attr("uid", "        pattern: '('\n"),
		"unknown setting":     attr("uid", "        maximum: 3\n"),
		"misspelt setting":    attr("uid", "        maxlength: 3\n"),
		"hidden required":     attr("title", "        hidden: true\n"),
		"error without match": "types:\n  grafana_folder: {}\nerrors:\n  - code: conflict\n    status: 409\n",
		"error without code":  "types:\n  grafana_folder: {}\nerrors:\n  - match: x\n    status: 409\n",
		"error not an error":  "types:\n  grafana_folder: {}\nerrors:\n  - code: conflict\n    match: x\n    status: 200\n",
		"error bad regexp":    "types:\n  grafana_folder: {}\nerrors:\n  - code: conflict\n    match: '('\n    status: 409\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := testParseRules(src, types)
			assert.Error(t, err)
		})
	}
}

func TestParseRulesEmpty(t *testing.T) {
	rs, errs, err := testParseRules("types:\n  grafana_folder: {}\n", map[string]tofu.Block{"grafana_folder": folder})
	require.NoError(t, err)
	assert.Empty(t, rs["grafana_folder"])
	assert.Empty(t, errs)
}

func TestEval(t *testing.T) {
	for source, want := range map[string]any{
		`spec.title.replace(" ", "-")`:                   "Platform-Team",
		`spec.title.lowerAscii().replace(" ", "-")`:      "platform-team",
		`"%s-%s".format([ns, spec.title.split(" ")[0]])`: "acme-Platform",
		`has(spec.uid) ? spec.uid : "none"`:              "none",
		`regex.replace(spec.title, "[^a-zA-Z]", "_")`:    "Platform_Team",
		`spec.size + 1.0`:                                float64(3),
	} {
		e, err := compile(defaultEnv, source)
		require.NoError(t, err, source)
		got, err := e.eval("acme", map[string]any{"title": "Platform Team", "size": float64(2)})
		require.NoError(t, err, source)
		assert.Equal(t, want, got, source)
	}
}

// The base path gives the contract's paths, with its namespace parameter.
func TestBasePath(t *testing.T) {
	a := testAPI(t)
	a.basePath = "/infra/{namespace}/stacks"
	paths := a.document()["paths"].(object)
	assert.Contains(t, paths, "/infra/{namespace}/stacks/folder")
	assert.Contains(t, paths, "/infra/{namespace}/stacks/folder/{id}")
}

// The namespace parameter has the configured pattern.
func TestNamespacePattern(t *testing.T) {
	a := testAPI(t)
	a.namespacePattern = `^[A-Za-z0-9.]+$`
	parameters := a.document()["components"].(object)["parameters"].(object)
	assert.Equal(t, `^[A-Za-z0-9.]+$`, parameters["namespace"].(object)["schema"].(object)["pattern"])
}

// With auth, every operation needs a bearer token; without, none.
func TestBearer(t *testing.T) {
	a := testAPI(t)
	assert.NotContains(t, a.document(), "security")
	a.bearer = true
	doc := a.document()
	assert.Equal(t, []any{object{"bearer": []string{}}}, doc["security"])
	schemes := doc["components"].(object)["securitySchemes"].(object)
	assert.Equal(t, "bearer", schemes["bearer"].(object)["scheme"])
}
