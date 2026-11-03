package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	infraerrors "github.com/infrapi/lib/pkg/errors"
)

// fakeProcess is a provider process of a namespace.
type fakeProcess struct {
	*fakeProvider
	namespace string
	closed    bool
}

func (f *fakeProcess) Close() error {
	f.closed = true
	return nil
}

// testPool is a pool whose processes are fakes, on a clock the test moves.
type testPool struct {
	*pool
	mu      sync.Mutex
	started []*fakeProcess
	clock   time.Time
}

func newTestPool(t namespacesConfig) *testPool {
	tp := &testPool{clock: time.Unix(0, 0)}
	tp.pool = newPool(func(_ context.Context, namespace string) (process, error) {
		if namespace == "unknown" {
			return nil, infraerrors.NotFound("unknown namespace %q", namespace)
		}
		tp.mu.Lock()
		defer tp.mu.Unlock()
		p := &fakeProcess{fakeProvider: newFakeProvider(), namespace: namespace}
		tp.started = append(tp.started, p)
		return p, nil
	}, t, 2)
	tp.now = func() time.Time { return tp.clock }
	return tp
}

func (tp *testPool) use(t *testing.T, namespace string) *fakeProcess {
	t.Helper()
	p, release, err := tp.acquire(context.Background(), namespace)
	require.NoError(t, err)
	release()
	return fakeOf(p)
}

// fakeOf is the fake behind the provider the pool gives.
func fakeOf(p provider) *fakeProcess { return p.(*watched).p.(*fakeProcess) }

func (tp *testPool) wait(d time.Duration) { tp.clock = tp.clock.Add(d) }

var testNamespaces = namespacesConfig{Idle: 10 * time.Minute, MaxAge: time.Hour, MaxProviders: 2}

func TestPoolProcessPerNamespace(t *testing.T) {
	tp := newTestPool(testNamespaces)
	acme := tp.use(t, "acme")
	assert.Equal(t, "acme", acme.namespace)
	assert.Same(t, acme, tp.use(t, "acme"), "the namespace's process serves its next request")
	assert.NotSame(t, acme, tp.use(t, "beta"), "another namespace, another process")
	assert.Len(t, tp.started, 2)
}

func TestPoolStartsOnce(t *testing.T) {
	tp := newTestPool(testNamespaces)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { tp.use(t, "acme") })
	}
	wg.Wait()
	assert.Len(t, tp.started, 1, "requests at once wait for the same process")
}

func TestPoolStartFails(t *testing.T) {
	tp := newTestPool(testNamespaces)
	_, _, err := tp.acquire(context.Background(), "unknown")
	var e *infraerrors.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, http.StatusNotFound, e.Code)
	assert.Empty(t, tp.namespaces, "a process that failed to start leaves the pool")
}

func TestPoolIdle(t *testing.T) {
	tp := newTestPool(testNamespaces)
	acme := tp.use(t, "acme")
	tp.wait(5 * time.Minute)
	tp.sweep()
	assert.False(t, acme.closed, "not idle long enough")

	tp.wait(11 * time.Minute)
	tp.sweep()
	assert.True(t, acme.closed)
	assert.NotSame(t, acme, tp.use(t, "acme"), "started again on the next request")
}

// A process too old stops once its requests are done, so that the namespace's
// credentials are read again.
func TestPoolMaxAge(t *testing.T) {
	tp := newTestPool(testNamespaces)
	acme := tp.use(t, "acme")
	p, release, err := tp.acquire(context.Background(), "acme")
	require.NoError(t, err)

	for range 7 {
		tp.wait(9 * time.Minute)
		tp.use(t, "acme")
	}
	assert.Same(t, acme, fakeOf(p))
	assert.False(t, acme.closed, "a request holds it")
	again := tp.use(t, "acme")
	assert.NotSame(t, acme, again, "a new process for new requests")

	release()
	assert.True(t, acme.closed, "the old one stops with its last request")
	assert.False(t, again.closed)
}

func TestPoolMaxProviders(t *testing.T) {
	tp := newTestPool(testNamespaces)
	acme := tp.use(t, "acme")
	tp.wait(time.Minute)
	beta := tp.use(t, "beta")

	tp.use(t, "gamma")
	assert.True(t, acme.closed, "the least recently used stopped to make room")
	assert.False(t, beta.closed)

	_, holdBeta, err := tp.acquire(context.Background(), "beta")
	require.NoError(t, err)
	defer holdBeta()
	_, holdGamma, err := tp.acquire(context.Background(), "gamma")
	require.NoError(t, err)
	defer holdGamma()
	_, _, err = tp.acquire(context.Background(), "delta")
	var e *infraerrors.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, http.StatusServiceUnavailable, e.Code, "every process is busy")
	require.NotNil(t, e.Retryable)
	assert.True(t, *e.Retryable)
}

// A namespace's requests beyond the parallelism wait, other namespaces' do not.
func TestPoolParallelismPerNamespace(t *testing.T) {
	tp := newTestPool(testNamespaces)
	for range 2 {
		_, release, err := tp.acquire(context.Background(), "acme")
		require.NoError(t, err)
		defer release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := tp.acquire(ctx, "acme")
	assert.ErrorContains(t, err, `all 2 calls to the provider of namespace "acme" were taken`)
	tp.use(t, "beta")
}

func TestPoolClose(t *testing.T) {
	tp := newTestPool(testNamespaces)
	acme, beta := tp.use(t, "acme"), tp.use(t, "beta")
	require.NoError(t, tp.Close())
	assert.True(t, acme.closed)
	assert.True(t, beta.closed)
}

// A process that no longer answers leaves the pool: it stops with the
// request that found it gone, and the next request starts another.
func TestPoolProcessGone(t *testing.T) {
	tp := newTestPool(testNamespaces)
	acme := tp.use(t, "acme")
	acme.err = infraerrors.Wrap(status.Error(codes.Unavailable, "connection refused"), http.StatusBadGateway, "tofu.Provider.Create", "apply")

	p, release, err := tp.acquire(context.Background(), "acme")
	require.NoError(t, err)
	_, err = p.Create(context.Background(), "grafana_folder", map[string]any{})
	require.Error(t, err)
	assert.False(t, acme.closed, "the request still holds it")
	release()
	assert.True(t, acme.closed)
	assert.NotSame(t, acme, tp.use(t, "acme"))

	beta := tp.use(t, "beta")
	beta.err = infraerrors.Upstream(false, "Grafana said no")
	p, release, err = tp.acquire(context.Background(), "beta")
	require.NoError(t, err)
	_, err = p.Create(context.Background(), "grafana_folder", map[string]any{})
	require.Error(t, err)
	release()
	assert.False(t, beta.closed, "an error of the provider's API is not the process gone")
}

func TestPoolStatus(t *testing.T) {
	tp := newTestPool(testNamespaces)
	beta := tp.use(t, "beta")
	beta.err = infraerrors.Upstream(false, "Grafana said no")
	p, release, err := tp.acquire(context.Background(), "beta")
	require.NoError(t, err)
	_, err = p.Create(context.Background(), "grafana_folder", map[string]any{})
	require.Error(t, err)
	_, err = p.Create(context.Background(), "grafana_folder", map[string]any{})
	require.Error(t, err)
	release()
	tp.wait(time.Minute)
	_, release, err = tp.acquire(context.Background(), "acme")
	require.NoError(t, err)
	defer release()

	gin.SetMode(gin.TestMode)
	e := gin.New()
	tp.routes(e)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/-/providers", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		MaxProviders int              `json:"max_providers"`
		Providers    []providerStatus `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 2, body.MaxProviders)
	require.Len(t, body.Providers, 2)
	assert.Equal(t, "acme", body.Providers[0].Namespace, "by namespace")
	assert.Equal(t, "running", body.Providers[0].State)
	assert.Equal(t, 1, body.Providers[0].InFlight)
	assert.Equal(t, 0, body.Providers[0].Requests, "served once released")
	assert.Equal(t, 0, body.Providers[1].InFlight)
	assert.Equal(t, 2, body.Providers[1].Requests)
	assert.Equal(t, 1, body.Providers[1].Errors, "one per request, however many of its calls failed")
	assert.True(t, body.Providers[1].LastUsed.Before(body.Providers[0].LastUsed))
}

// The metrics follow the processes: running, started and stopped, by
// reason.
func TestPoolMetrics(t *testing.T) {
	running := testutil.ToFloat64(providersRunning)
	ok := testutil.ToFloat64(providerStarts.WithLabelValues("ok"))
	failed := testutil.ToFloat64(providerStarts.WithLabelValues("failed"))
	idle := testutil.ToFloat64(providerStops.WithLabelValues(stopIdle))
	shutdown := testutil.ToFloat64(providerStops.WithLabelValues(stopShutdown))

	namespaces := testNamespaces
	namespaces.MaxProviders = 3 // the unknown namespace stops no one
	tp := newTestPool(namespaces)
	tp.use(t, "acme")
	tp.use(t, "beta")
	_, _, err := tp.acquire(context.Background(), "unknown")
	require.Error(t, err)
	assert.Equal(t, running+2, testutil.ToFloat64(providersRunning))
	assert.Equal(t, ok+2, testutil.ToFloat64(providerStarts.WithLabelValues("ok")))
	assert.Equal(t, failed+1, testutil.ToFloat64(providerStarts.WithLabelValues("failed")))

	tp.wait(11 * time.Minute)
	tp.use(t, "beta")
	tp.sweep()
	assert.Equal(t, running+1, testutil.ToFloat64(providersRunning))
	assert.Equal(t, idle+1, testutil.ToFloat64(providerStops.WithLabelValues(stopIdle)))

	require.NoError(t, tp.Close())
	assert.Equal(t, running, testutil.ToFloat64(providersRunning))
	assert.Equal(t, shutdown+1, testutil.ToFloat64(providerStops.WithLabelValues(stopShutdown)))
}
