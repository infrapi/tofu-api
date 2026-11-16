package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infrapi/lib/pkg/auth"
)

const folderPath = "/grafana/v1/namespaces/acme/folder"

// A dry run of an update plans the spec against the folder as it is now:
// changed by hand, the change is its drift; nothing is applied.
func TestDryRunUpdateShowsDrift(t *testing.T) {
	p := newFakeProvider()
	e := newTestEngine(t, p)
	code, body := do(t, e, http.MethodPost, folderPath, `{"title":"Ops"}`)
	require.Equal(t, http.StatusCreated, code)
	id := body["id"].(string)

	p.objects[id]["title"] = "Renamed by hand"

	code, body = do(t, e, http.MethodPut, folderPath+"/"+id+"?dryRun=true", `{"title":"Ops"}`)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "update", body["action"])
	assert.Equal(t, []any{map[string]any{"path": "title", "current": "Renamed by hand", "planned": "Ops"}}, body["changes"])
	assert.Equal(t, "Renamed by hand", body["current"].(map[string]any)["title"])
	assert.Equal(t, "Ops", body["planned"].(map[string]any)["title"])
	assert.Equal(t, "Renamed by hand", p.objects[id]["title"], "nothing applied")

	code, body = do(t, e, http.MethodPut, folderPath+"/"+id+"?dryRun=true", `{"title":"Renamed by hand"}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "none", body["action"])
	assert.Equal(t, []any{}, body["changes"])
}

// A dry run of a create says what would be created, and creates nothing;
// what the caller does not see, the org a rule sets, is not in the plan.
func TestDryRunCreate(t *testing.T) {
	p := newFakeProvider()
	e := newTestEngine(t, p)

	code, body := do(t, e, http.MethodPost, folderPath+"?dryRun=true", `{"title":"Ops"}`)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "create", body["action"])
	assert.Equal(t, []any{"id"}, body["unknown"])
	assert.Empty(t, p.objects, "nothing created")
	for _, c := range body["changes"].([]any) {
		assert.NotEqual(t, "org_id", c.(map[string]any)["path"], "hidden")
	}
	assert.NotContains(t, body["planned"], "org_id")
	assert.NotContains(t, body, "current")
}

// An unknown id is a 404, as for a GET: for a reconciliation, a resource
// deleted by hand.
func TestDryRunUnknownID(t *testing.T) {
	e := newTestEngine(t, newFakeProvider())
	code, _ := do(t, e, http.MethodPut, folderPath+"/404?dryRun=true", `{"title":"Ops"}`)
	assert.Equal(t, http.StatusNotFound, code)
}

// ignore_drift leaves an attribute out of the changes: with nothing else
// changing, there is no drift.
func TestDryRunIgnoreDrift(t *testing.T) {
	p := newFakeProvider()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	a := testAPI(t)
	rs := a.rules["grafana_folder"]
	rs["title"] = rule{IgnoreDrift: true}
	newHandlers(a, newShared(p, 10), time.Minute).routes(e)

	code, body := do(t, e, http.MethodPost, folderPath, `{"title":"Ops"}`)
	require.Equal(t, http.StatusCreated, code)
	id := body["id"].(string)
	p.objects[id]["title"] = "Renamed by hand"

	code, body = do(t, e, http.MethodPut, folderPath+"/"+id+"?dryRun=true", `{"title":"Ops"}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "none", body["action"])
}

// A dry run changes nothing: the RBAC is asked for get, not create or update.
func TestDryRunIsAGet(t *testing.T) {
	rbac := &fakeRBAC{allow: func(q auth.AuthorizeRequest) bool { return q.Verb == "get" }}
	e := newGuardedEngine(t, newFakeProvider(), rbac)

	code, _ := doAs(t, e, "tok", http.MethodPost, folderPath+"?dryRun=true", `{"title":"Ops"}`)
	assert.Equal(t, http.StatusOK, code)
	code, _ = doAs(t, e, "tok", http.MethodPost, folderPath, `{"title":"Ops"}`)
	assert.Equal(t, http.StatusForbidden, code, "a real create is still a create")
}
