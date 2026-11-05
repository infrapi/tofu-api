package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infrapi/lib/pkg/auth"
	"github.com/infrapi/lib/pkg/config"
	"github.com/infrapi/lib/pkg/server"
)

// fakeRBAC allows what allow says, and keeps every question it was asked.
type fakeRBAC struct {
	allow  func(auth.AuthorizeRequest) bool
	asked  []auth.AuthorizeRequest
	tokens []string
}

func (f *fakeRBAC) Authorize(_ context.Context, token string, req auth.AuthorizeRequest) (bool, string, error) {
	f.asked = append(f.asked, req)
	f.tokens = append(f.tokens, token)
	if f.allow(req) {
		return true, "", nil
	}
	return false, "your role does not grant this action", nil
}

func newGuardedEngine(t *testing.T, p provider, rbac accessChecker) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := newHandlers(testAPI(t), newShared(p, 10), time.Minute)
	h.access = &access{checker: rbac, api: "grafana.example.com"}
	h.routes(engine)
	return engine
}

func doAs(t *testing.T, e *gin.Engine, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec.Code, out
}

// Every call asks the RBAC with the method's verb, the resource type, the
// path's namespace and the API group, and the token it came with.
func TestAuthorizeAsksTheRBAC(t *testing.T) {
	rbac := &fakeRBAC{allow: func(auth.AuthorizeRequest) bool { return true }}
	e := newGuardedEngine(t, newFakeProvider(), rbac)
	const collection = "/grafana/v1/namespaces/acme/folder"

	code, body := doAs(t, e, "tok", http.MethodPost, collection, `{"title":"Ops"}`)
	require.Equal(t, http.StatusCreated, code)
	id := body["id"].(string)
	code, _ = doAs(t, e, "tok", http.MethodGet, collection+"/"+id, "")
	require.Equal(t, http.StatusOK, code)
	code, _ = doAs(t, e, "tok", http.MethodPut, collection+"/"+id, `{"title":"Operations"}`)
	require.Equal(t, http.StatusOK, code)
	code, _ = doAs(t, e, "tok", http.MethodDelete, collection+"/"+id, "")
	require.Equal(t, http.StatusNoContent, code)

	var verbs []string
	for _, q := range rbac.asked {
		assert.Equal(t, "acme", q.Namespace)
		assert.Equal(t, "grafana.example.com", q.Api)
		assert.Equal(t, "folder", q.Resource)
		verbs = append(verbs, q.Verb)
	}
	assert.Equal(t, []string{"create", "get", "update", "delete"}, verbs)
	assert.Equal(t, []string{"tok", "tok", "tok", "tok"}, rbac.tokens)
}

// A refusal is a 403 with the RBAC's reason, before the provider is called.
func TestAuthorizeRefuses(t *testing.T) {
	rbac := &fakeRBAC{allow: func(q auth.AuthorizeRequest) bool { return q.Verb != "create" }}
	p := newFakeProvider()
	e := newGuardedEngine(t, p, rbac)

	code, body := doAs(t, e, "tok", http.MethodPost, "/grafana/v1/namespaces/acme/folder", `{"title":"Ops"}`)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body["detail"], `create folder in namespace "acme": your role does not grant this action`)
	assert.Empty(t, p.objects, "the provider is not called")
}

// Without a bearer token, a 401: the RBAC is not asked.
func TestAuthorizeWithoutToken(t *testing.T) {
	rbac := &fakeRBAC{allow: func(auth.AuthorizeRequest) bool { return true }}
	e := newGuardedEngine(t, newFakeProvider(), rbac)

	code, _ := doAs(t, e, "", http.MethodGet, "/grafana/v1/namespaces/acme/folder/1", "")
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Empty(t, rbac.asked)
}

// authService stands in for the authentication service: "good" is jdoe's
// token, whom the RBAC allows in acme only. checked keeps its questions.
func authService(t *testing.T, checked *[]auth.AuthorizeRequest) *httptest.Server {
	t.Helper()
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case auth.UserinfoPath:
			_, err := w.Write([]byte(`{"sub":"jdoe","groups":["sre"]}`))
			assert.NoError(t, err)
		case auth.AuthorizePath:
			var q auth.AuthorizeRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&q))
			*checked = append(*checked, q)
			_, err := w.Write([]byte(`{"allowed":` + map[bool]string{true: "true", false: "false"}[q.Namespace == "acme"] + `}`))
			assert.NoError(t, err)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(svc.Close)
	return svc
}

// With pkg/auth's client against a stand-in of the authentication
// service: the token is resolved, then the RBAC decides.
func TestAuthService(t *testing.T) {
	var checked []auth.AuthorizeRequest
	svc := authService(t, &checked)

	client, err := auth.NewClient(auth.ClientOptions{BaseURL: svc.URL})
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(client.Middleware())
	h := newHandlers(testAPI(t), newShared(newFakeProvider(), 10), time.Minute)
	h.access = &access{checker: client, api: "grafana.example.com"}
	h.routes(e)

	code, _ := doAs(t, e, "bad", http.MethodPost, "/grafana/v1/namespaces/acme/folder", `{"title":"Ops"}`)
	assert.Equal(t, http.StatusUnauthorized, code)
	code, _ = doAs(t, e, "good", http.MethodPost, "/grafana/v1/namespaces/acme/folder", `{"title":"Ops"}`)
	assert.Equal(t, http.StatusCreated, code)
	code, _ = doAs(t, e, "good", http.MethodPost, "/grafana/v1/namespaces/beta/folder", `{"title":"Ops"}`)
	assert.Equal(t, http.StatusForbidden, code)
	require.Len(t, checked, 2)
	assert.Equal(t, auth.AuthorizeRequest{Namespace: "acme", Api: "grafana.example.com", Resource: "folder", Verb: "create"}, checked[0])
}

// As serve sets it up: the server of infrapi/lib validates requests
// against a contract that requires a bearer token, authenticates them,
// then the RBAC decides; the operational endpoints stay open.
func TestAuthThroughTheServer(t *testing.T) {
	var checked []auth.AuthorizeRequest
	svc := authService(t, &checked)
	client, err := auth.NewClient(auth.ClientOptions{BaseURL: svc.URL})
	require.NoError(t, err)

	t.Setenv("INFRAPI_CONFIG_DOTENV_FILE", "packaging/example.env")
	vars, err := config.NewConfig()
	require.NoError(t, err)
	cfg, err := vars.GetAppConfig()
	require.NoError(t, err)

	a := testAPI(t)
	a.bearer = true
	contract, err := json.Marshal(a.document())
	require.NoError(t, err)
	e, err := server.NewServerGin(cfg,
		server.WithOpenAPI(server.OpenAPISpec{Name: "grafana v1", Path: "grafana/v1.json", Content: contract}),
		server.WithAuth(client.Middleware()))
	require.NoError(t, err)
	h := newHandlers(a, newShared(newFakeProvider(), 10), time.Minute)
	h.access = &access{checker: client, api: "grafana.example.com"}
	h.routes(e)

	const folder = `{"title":"Ops","permission":[{"role":"Viewer"}]}`
	code, _ := doAs(t, e, "", http.MethodPost, "/grafana/v1/namespaces/acme/folder", folder)
	assert.Equal(t, http.StatusUnauthorized, code)
	code, body := doAs(t, e, "good", http.MethodPost, "/grafana/v1/namespaces/acme/folder", folder)
	assert.Equal(t, http.StatusCreated, code, body)
	code, _ = doAs(t, e, "good", http.MethodPost, "/grafana/v1/namespaces/beta/folder", folder)
	assert.Equal(t, http.StatusForbidden, code)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/-/openapi/grafana/v1.json", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "the contract without a token")
}
