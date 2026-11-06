package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	infraerrors "github.com/infrapi/lib/pkg/errors"
	"github.com/infrapi/lib/pkg/logger"
	"github.com/infrapi/lib/pkg/server"
	"github.com/infrapi/lib/pkg/tofu"
)

// providers gives the handlers the provider that serves a namespace, and a slot
// at it: release gives the slot back.
type providers interface {
	acquire(ctx context.Context, namespace string) (p provider, release func(), err error)
}

// shared is one provider, configured once, for every namespace.
type shared struct {
	p provider
	// slots bounds the requests at the provider at once, as Terraform's
	// -parallelism does: the provider would pass them all on to its API.
	slots chan struct{}
}

func newShared(p provider, parallelism int) *shared {
	return &shared{p: p, slots: make(chan struct{}, parallelism)}
}

func (s *shared) acquire(ctx context.Context, _ string) (provider, func(), error) {
	select {
	case s.slots <- struct{}{}:
		return s.p, func() { <-s.slots }, nil
	case <-ctx.Done():
		return nil, nil, infraerrors.Unavailable("the caller left while all %d calls to the provider were taken", cap(s.slots))
	}
}

// process is a provider in a process of its own, as *tofu.Provider is.
type process interface {
	provider
	Close() error
}

// pool runs a provider process per namespace, configured with the namespace's own
// credentials: what one namespace's provider can reach is what its credentials
// allow, whatever the request says. A process starts on its namespace's first
// request, and stops once idle, or once too old so that rotated credentials
// are read again; when max are running, the least recently used idle one
// stops to make room.
type pool struct {
	start       func(ctx context.Context, namespace string) (process, error)
	parallelism int
	max         int
	idle        time.Duration
	maxAge      time.Duration
	now         func() time.Time

	mu         sync.Mutex
	namespaces map[string]*instance
}

// instance is the provider process of a namespace. Its fields but ready, p,
// err and slots are the pool's, under its lock.
type instance struct {
	namespace string
	ready     chan struct{} // closed once the process started, or failed to
	p         process
	err       error
	slots     chan struct{}

	started, used time.Time
	inFlight      int    // requests holding or waiting for the instance
	requests      int    // requests it served
	errors        int    // of those, the ones the provider failed
	retired       bool   // out of the pool: closed when its last call ends
	reason        string // why it was retired: a stopReason
}

func newPool(start func(context.Context, string) (process, error), t namespacesConfig, parallelism int) *pool {
	return &pool{
		start: start, parallelism: parallelism, max: t.MaxProviders, idle: t.Idle, maxAge: t.MaxAge,
		now: time.Now, namespaces: map[string]*instance{},
	}
}

func (pl *pool) acquire(ctx context.Context, namespace string) (provider, func(), error) {
	in, err := pl.take(namespace)
	if err != nil {
		return nil, nil, err
	}
	select {
	case <-in.ready:
	case <-ctx.Done():
		pl.release(in)
		return nil, nil, infraerrors.Unavailable("the caller left while the provider of namespace %q started", namespace)
	}
	if in.err != nil {
		pl.release(in)
		return nil, nil, in.err
	}
	select {
	case in.slots <- struct{}{}:
		w := &watched{p: in.p, pl: pl, in: in}
		return w, func() { <-in.slots; pl.served(in, w.failed) }, nil
	case <-ctx.Done():
		pl.release(in)
		return nil, nil, infraerrors.Unavailable("the caller left while all %d calls to the provider of namespace %q were taken", pl.parallelism, namespace)
	}
}

// take returns the namespace's instance, starting one when there is none, and
// counts the call on it.
func (pl *pool) take(namespace string) (*instance, error) {
	pl.mu.Lock()
	var stop []*instance
	defer func() { pl.mu.Unlock(); pl.close(stop) }()

	now := pl.now()
	in := pl.namespaces[namespace]
	if in != nil && pl.maxAge > 0 && started(in) && now.Sub(in.started) > pl.maxAge {
		stop = pl.retire(in, stop, stopMaxAge)
		in = nil
	}
	if in == nil {
		if len(pl.namespaces) >= pl.max {
			lru := pl.leastRecentlyUsedIdle()
			if lru == nil {
				return nil, infraerrors.Unavailable("all %d provider processes are busy with other namespaces", pl.max)
			}
			stop = pl.retire(lru, stop, stopEvicted)
		}
		in = &instance{namespace: namespace, ready: make(chan struct{}), slots: make(chan struct{}, pl.parallelism), started: now}
		pl.namespaces[namespace] = in
		go pl.launch(in)
	}
	in.inFlight++
	in.used = now
	return in, nil
}

// launch starts the instance's process, bounded by the time a change may
// take; a process that fails to start leaves the pool, and the namespace's
// next request tries again.
func (pl *pool) launch(in *instance) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	in.p, in.err = pl.start(ctx, in.namespace)
	if in.err != nil {
		pl.mu.Lock()
		if pl.namespaces[in.namespace] == in {
			delete(pl.namespaces, in.namespace)
		}
		in.retired = true
		pl.mu.Unlock()
		providerStarts.WithLabelValues("failed").Inc()
		logger.L().Warn("provider of a namespace failed to start", logger.String("namespace", in.namespace), logger.Err(in.err))
	} else {
		providerStarts.WithLabelValues("ok").Inc()
		providersRunning.Inc()
		logger.L().Info("provider of a namespace started", logger.String("namespace", in.namespace))
	}
	close(in.ready)
}

// watched is the provider of an instance for one request, which leaves the
// pool when a call finds its process gone: the namespace's next request starts
// another.
type watched struct {
	p      provider
	pl     *pool
	in     *instance
	failed bool // a call failed: the request counts as an error
}

func (w *watched) Create(ctx context.Context, resourceType string, config map[string]any) (tofu.Resource, error) {
	res, err := w.p.Create(ctx, resourceType, config)
	return res, w.check(err)
}

func (w *watched) Import(ctx context.Context, resourceType, id string) (tofu.Resource, error) {
	res, err := w.p.Import(ctx, resourceType, id)
	return res, w.check(err)
}

func (w *watched) Update(ctx context.Context, state tofu.Resource, config map[string]any) (tofu.Resource, error) {
	res, err := w.p.Update(ctx, state, config)
	return res, w.check(err)
}

func (w *watched) Delete(ctx context.Context, state tofu.Resource) error {
	return w.check(w.p.Delete(ctx, state))
}

func (w *watched) ReadDataSource(ctx context.Context, dataSource string, config map[string]any) (map[string]any, error) {
	attrs, err := w.p.ReadDataSource(ctx, dataSource, config)
	return attrs, w.check(err)
}

// check retires the instance when err says its process does not answer:
// gRPC's Unavailable, as when the process died. What the provider says of
// its API comes as diagnostics, not as this.
func (w *watched) check(err error) error {
	if err != nil {
		w.failed = true
	}
	if s, ok := status.FromError(err); err == nil || !ok || s.Code() != codes.Unavailable {
		return err
	}
	w.pl.mu.Lock()
	if w.pl.namespaces[w.in.namespace] == w.in {
		logger.L().Warn("provider of a namespace does not answer: replaced on the next request", logger.String("namespace", w.in.namespace), logger.Err(err))
		w.pl.retire(w.in, nil, stopGone) // the call holds it: its release stops it
	}
	w.pl.mu.Unlock()
	return err
}

// served counts a request the instance served, then releases it.
func (pl *pool) served(in *instance, failed bool) {
	pl.mu.Lock()
	in.requests++
	if failed {
		in.errors++
	}
	pl.mu.Unlock()
	pl.release(in)
}

func (pl *pool) release(in *instance) {
	pl.mu.Lock()
	var stop []*instance
	in.inFlight--
	in.used = pl.now()
	if in.retired && in.inFlight == 0 {
		stop = append(stop, in)
	}
	pl.mu.Unlock()
	pl.close(stop)
}

// sweep stops the processes idle for too long, or too old, that no request
// holds.
func (pl *pool) sweep() {
	pl.mu.Lock()
	var stop []*instance
	now := pl.now()
	for _, in := range pl.namespaces {
		if in.inFlight > 0 || !started(in) {
			continue
		}
		switch {
		case now.Sub(in.used) > pl.idle:
			stop = pl.retire(in, stop, stopIdle)
		case pl.maxAge > 0 && now.Sub(in.started) > pl.maxAge:
			stop = pl.retire(in, stop, stopMaxAge)
		}
	}
	pl.mu.Unlock()
	pl.close(stop)
}

// sweepEvery sweeps until ctx is done.
func (pl *pool) sweepEvery(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pl.sweep()
		}
	}
}

// Close stops every process; the server stopped first, so no request holds
// one.
func (pl *pool) Close() error {
	pl.mu.Lock()
	var stop []*instance
	for _, in := range pl.namespaces {
		stop = pl.retire(in, stop, stopShutdown)
	}
	pl.mu.Unlock()
	var errs []error
	for _, in := range stop {
		<-in.ready
		if in.p != nil {
			errs = append(errs, in.p.Close())
			stopped(in)
		}
	}
	return errors.Join(errs...)
}

// retire takes the instance out of the pool, and adds it to stop when no
// call holds it; otherwise its last call stops it.
func (pl *pool) retire(in *instance, stop []*instance, reason string) []*instance {
	delete(pl.namespaces, in.namespace)
	in.retired, in.reason = true, reason
	if in.inFlight == 0 {
		stop = append(stop, in)
	}
	return stop
}

func (pl *pool) leastRecentlyUsedIdle() *instance {
	var lru *instance
	for _, in := range pl.namespaces {
		if in.inFlight == 0 && started(in) && (lru == nil || in.used.Before(lru.used)) {
			lru = in
		}
	}
	return lru
}

// close stops the processes of instances out of the pool, outside its lock.
func (pl *pool) close(stop []*instance) {
	for _, in := range stop {
		if in.p == nil {
			continue
		}
		err := in.p.Close()
		stopped(in)
		if err != nil {
			if in.reason == stopGone { // as when the process died: nothing left to stop
				logger.L().Info("provider of a namespace was already stopped", logger.String("namespace", in.namespace), logger.Err(err))
				continue
			}
			logger.L().Error("provider of a namespace failed to stop", logger.String("namespace", in.namespace), logger.Err(err))
			continue
		}
		logger.L().Info("provider of a namespace stopped", logger.String("namespace", in.namespace))
	}
}

// Why an instance stops, as tofu_api_provider_stops_total counts them.
const (
	stopIdle     = "idle"
	stopMaxAge   = "max_age"
	stopEvicted  = "evicted"
	stopGone     = "gone"
	stopShutdown = "shutdown"
)

// stopped counts the instance's process as stopped.
func stopped(in *instance) {
	providersRunning.Dec()
	providerStops.WithLabelValues(in.reason).Inc()
}

func started(in *instance) bool {
	select {
	case <-in.ready:
		return true
	default:
		return false
	}
}

// providerStatus is a namespace's provider process, as GET /-/providers lists
// it.
type providerStatus struct {
	Namespace string    `json:"namespace"`
	State     string    `json:"state"` // starting, running
	Started   time.Time `json:"started"`
	LastUsed  time.Time `json:"last_used"`
	InFlight  int       `json:"in_flight"` // requests holding or waiting for it
	Requests  int       `json:"requests"`  // requests it served
	Errors    int       `json:"errors"`    // of those, the ones the provider failed
}

// status lists the processes that serve their namespace's requests, by namespace;
// those stopping once their last call ends are not.
func (pl *pool) status() []providerStatus {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	list := make([]providerStatus, 0, len(pl.namespaces))
	for _, in := range pl.namespaces {
		state := "starting"
		if started(in) {
			state = "running"
		}
		list = append(list, providerStatus{Namespace: in.namespace, State: state, Started: in.started, LastUsed: in.used,
			InFlight: in.inFlight, Requests: in.requests, Errors: in.errors,
		})
	}
	slices.SortFunc(list, func(a, b providerStatus) int { return strings.Compare(a.Namespace, b.Namespace) })
	return list
}

// providersOperation documents GET /-/providers in the specification of the
// operational endpoints.
var providersOperation = server.Operation{
	Method: http.MethodGet, Path: "/-/providers", Summary: "The namespaces' provider processes",
	Responses: map[string]server.Response{
		"200": {Description: "The processes serving their namespace's requests, by namespace", ContentType: "application/json",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"max_providers": map[string]any{"type": "integer", "description": "processes running at most"},
					"providers": map[string]any{"type": "array", "items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"namespace": map[string]any{"type": "string"},
							"state":     map[string]any{"type": "string", "enum": []string{"starting", "running"}},
							"started":   map[string]any{"type": "string", "format": "date-time"},
							"last_used": map[string]any{"type": "string", "format": "date-time"},
							"in_flight": map[string]any{"type": "integer", "description": "requests holding or waiting for it"},
							"requests":  map[string]any{"type": "integer", "description": "requests it served"},
							"errors":    map[string]any{"type": "integer", "description": "of those, the ones the provider failed"},
						},
					}},
				},
			}},
	},
}

// routes adds GET /-/providers, the namespaces' processes, beside the
// server's own /-/ endpoints.
func (pl *pool) routes(r gin.IRouter) {
	r.GET("/-/providers", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"max_providers": pl.max, "providers": pl.status()})
	})
}
