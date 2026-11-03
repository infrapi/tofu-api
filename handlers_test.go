package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infraerrors "github.com/infrapi/lib/pkg/errors"
	"github.com/infrapi/lib/pkg/tofu"
)

// fakeProvider stands for the system the provider drives: it holds the
// resources by id, which the service itself must not.
type fakeProvider struct {
	next    int
	objects map[string]map[string]any
	err     error                     // returned by Create, when set
	stores  map[string]any            // what Create stores instead of what it is given
	create  func(ctx context.Context) // called as Create starts, when set
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{objects: map[string]map[string]any{}}
}

func (f *fakeProvider) Create(ctx context.Context, rt string, config map[string]any) (tofu.Resource, error) {
	if f.create != nil {
		f.create(ctx)
	}
	if f.err != nil {
		return tofu.Resource{}, f.err
	}
	f.next++
	id := fmt.Sprint(f.next)
	attrs := maps.Clone(config)
	maps.Copy(attrs, f.stores)
	attrs["id"] = id
	f.objects[id] = attrs
	return tofu.Resource{ResourceType: rt, Attributes: attrs}, nil
}

// Import answers from the id alone; an unknown id is a 404, as pkg/tofu
// makes it.
func (f *fakeProvider) Import(_ context.Context, rt, id string) (tofu.Resource, error) {
	attrs, ok := f.objects[id]
	if !ok {
		return tofu.Resource{}, infraerrors.NotFound("no %s with id %q", rt, id)
	}
	return tofu.Resource{ResourceType: rt, Attributes: maps.Clone(attrs)}, nil
}

func (f *fakeProvider) Update(_ context.Context, state tofu.Resource, config map[string]any) (tofu.Resource, error) {
	id := fmt.Sprint(state.Attributes["id"])
	attrs := maps.Clone(config)
	attrs["id"] = id
	f.objects[id] = attrs
	return tofu.Resource{ResourceType: state.ResourceType, Attributes: attrs}, nil
}

// Delete fails on a cancelled context, as a call to the provider would.
func (f *fakeProvider) Delete(ctx context.Context, state tofu.Resource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(f.objects, fmt.Sprint(state.Attributes["id"]))
	return nil
}

func newTestEngine(t *testing.T, p provider) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := newHandlers(testAPI(t), newShared(p, 10), time.Minute)
	h.routes(engine)
	return engine
}

func do(t *testing.T, e *gin.Engine, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec.Code, out
}

func TestLifecycle(t *testing.T) {
	p := newFakeProvider()
	e := newTestEngine(t, p)
	const collection = "/grafana/v1/namespaces/acme/folder"

	code, body := do(t, e, http.MethodPost, collection, `{"title":"Ops","token":"s3cret"}`)
	require.Equal(t, http.StatusCreated, code)
	id := body["id"].(string)
	assert.Equal(t, "acme", p.objects[id]["org_id"], "fixed attribute, ${namespace} replaced")
	assert.NotContains(t, body, "token", "sensitive values are not returned")
	assert.NotContains(t, body, "org_id", "hidden attributes are not returned")

	code, body = do(t, e, http.MethodPut, collection+"/"+id, `{"title":"Operations"}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "Operations", body["title"])
	assert.Equal(t, id, body["id"])

	code, body = do(t, e, http.MethodGet, collection+"/"+id, "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "Operations", body["title"])

	code, _ = do(t, e, http.MethodDelete, collection+"/"+id, "")
	assert.Equal(t, http.StatusNoContent, code)
	code, _ = do(t, e, http.MethodGet, collection+"/"+id, "")
	assert.Equal(t, http.StatusNotFound, code)
}

// Another namespace's id is not found, whatever the method: the caller sends
// the id, the service checks it is the namespace's.
func TestOtherNamespacesResource(t *testing.T) {
	p := newFakeProvider()
	e := newTestEngine(t, p)

	code, body := do(t, e, http.MethodPost, "/grafana/v1/namespaces/acme/folder", `{"title":"Ops"}`)
	require.Equal(t, http.StatusCreated, code)
	other := "/grafana/v1/namespaces/other/folder/" + body["id"].(string)

	code, _ = do(t, e, http.MethodGet, other, "")
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = do(t, e, http.MethodPut, other, `{"title":"Mine"}`)
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = do(t, e, http.MethodDelete, other, "")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Len(t, p.objects, 1)
}

// A provider that stores another value than the rule's, such as the default
// org for one it cannot parse, made a resource out of the namespace's reach:
// it is deleted, and the caller told.
func TestCreateNotKept(t *testing.T) {
	p := newFakeProvider()
	p.stores = map[string]any{"org_id": "1"}
	e := newTestEngine(t, p)

	code, body := do(t, e, http.MethodPost, "/grafana/v1/namespaces/acme/folder", `{"title":"Ops"}`)
	assert.Equal(t, http.StatusBadGateway, code)
	assert.Contains(t, body["detail"], `other values than the rules fix for namespace "acme"`)
	assert.Empty(t, p.objects)
}

// A caller that leaves while the provider creates gets no id: the create
// goes on, and the resource is deleted rather than left to no one.
func TestCallerLeavesDuringCreate(t *testing.T) {
	p := newFakeProvider()
	e := newTestEngine(t, p)
	ctx, leave := context.WithCancel(context.Background())
	p.create = func(provider context.Context) {
		leave()
		assert.NoError(t, provider.Err(), "the create goes on")
	}

	req := httptest.NewRequest(http.MethodPost, "/grafana/v1/namespaces/acme/folder", strings.NewReader(`{"title":"Ops"}`)).WithContext(ctx)
	e.ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, 1, p.next, "created")
	assert.Empty(t, p.objects, "then deleted")
}

// Requests beyond the provider's parallelism wait; one whose caller leaves
// while waiting never reaches the provider.
func TestParallelism(t *testing.T) {
	p := newFakeProvider()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	newHandlers(testAPI(t), newShared(p, 1), time.Minute).routes(e)

	started, finish := make(chan struct{}), make(chan struct{})
	p.create = func(context.Context) {
		close(started)
		<-finish
	}
	first := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/grafana/v1/namespaces/acme/folder", strings.NewReader(`{"title":"Ops"}`)))
		first <- rec.Code
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grafana/v1/namespaces/acme/folder/1", nil).WithContext(ctx))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "waited for the slot, then the caller left")

	close(finish)
	assert.Equal(t, http.StatusCreated, <-first)
}

func TestUnknownID(t *testing.T) {
	e := newTestEngine(t, newFakeProvider())
	code, _ := do(t, e, http.MethodGet, "/grafana/v1/namespaces/acme/folder/42", "")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestDefaults(t *testing.T) {
	p := newFakeProvider()
	e := newTestEngine(t, p)
	const collection = "/grafana/v1/namespaces/acme/folder"

	code, body := do(t, e, http.MethodPost, collection, `{"title":"Platform Team"}`)
	require.Equal(t, http.StatusCreated, code)
	id := body["id"].(string)
	assert.Equal(t, "Platform-Team", p.objects[id]["uid"], "derived from the title")

	code, _ = do(t, e, http.MethodPut, collection+"/"+id, `{"title":"Team"}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "Platform-Team", p.objects[id]["uid"], "an update keeps the uid the folder has")

	code, body = do(t, e, http.MethodPost, collection, `{"title":"Ops","uid":"ops"}`)
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "ops", p.objects[body["id"].(string)]["uid"], "the caller's value wins")

	code, body = do(t, e, http.MethodPost, collection, `{"title":"Ops/Prod"}`)
	assert.Equal(t, http.StatusBadRequest, code, "a derived value is checked like a sent one")
	assert.Contains(t, body["detail"], "does not match")
	assert.Len(t, p.objects, 2)
}

// A provider error is the HTTP error the first matching rule gives it, or a
// 502 upstream_error; an error pkg/tofu already classified is kept.
func TestProviderErrors(t *testing.T) {
	const origin = "tofu.Provider.Create(grafana.grafana_folder)"
	// diag is a provider's diagnostic as pkg/tofu returns it from a phase
	// other than validate or plan.
	diag := func(msg string) error {
		err := infraerrors.New(http.StatusBadGateway, origin, "%s", msg).WithSlug("upstream_error").WithTitle("Upstream error")
		err.Err = &tofu.DiagnosticsError{Phase: tofu.PhaseApply, Diagnostics: []tofu.Diagnostic{{Summary: msg}}}
		return err
	}
	// transport is the provider's process failing to answer.
	transport := func(msg string) error {
		return infraerrors.Wrap(errors.New(msg), http.StatusBadGateway, origin, "apply resource type %q", "grafana_folder")
	}
	invalid := infraerrors.New(http.StatusUnprocessableEntity, origin, "Invalid uid").WithSlug("invalid_configuration")
	replace := infraerrors.Wrap(tofu.ErrReplaceRequired, http.StatusConflict, origin, "changing uid requires replacing the resource").WithSlug("replace_required")
	for name, tc := range map[string]struct {
		err       error
		status    int
		code      string
		retryable bool
	}{
		"conflict":            {diag("failed to create folder: [POST /folders] createFolder (status 412): {}"), http.StatusConflict, "already_exists", false},
		"outage":              {diag(`Post "http://grafana/api/folders": dial tcp: connect: connection refused`), http.StatusServiceUnavailable, "upstream_unavailable", true},
		"transport":           {transport("connection refused"), http.StatusServiceUnavailable, "upstream_unavailable", true},
		"unmatched":           {diag("something else"), http.StatusBadGateway, "upstream_error", false},
		"unmatched transport": {transport("plugin exited"), http.StatusBadGateway, "upstream_error", false},
		"classified":          {infraerrors.Invalid("bad config"), http.StatusBadRequest, "invalid_request", false},
		"invalid":             {invalid, http.StatusUnprocessableEntity, "invalid_configuration", false},
		"replace":             {replace, http.StatusConflict, "replace_required", false},
	} {
		t.Run(name, func(t *testing.T) {
			p := newFakeProvider()
			p.err = tc.err
			code, body := do(t, newTestEngine(t, p), http.MethodPost, "/grafana/v1/namespaces/acme/folder", `{"title":"Ops"}`)
			assert.Equal(t, tc.status, code)
			assert.Equal(t, tc.code, body["code"])
			assert.Equal(t, tc.retryable, body["retryable"])
		})
	}
}
