package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	infraerrors "github.com/infrapi/lib/pkg/errors"
	"github.com/infrapi/lib/pkg/logger"
	"github.com/infrapi/lib/pkg/server"
	"github.com/infrapi/lib/pkg/tofu"
)

// provider is what the handlers need from a running provider; *tofu.Provider
// has it.
type provider interface {
	Create(ctx context.Context, resourceType string, config map[string]any) (tofu.Resource, error)
	Import(ctx context.Context, resourceType, id string) (tofu.Resource, error)
	Update(ctx context.Context, state tofu.Resource, config map[string]any) (tofu.Resource, error)
	Delete(ctx context.Context, state tofu.Resource) error
}

// handlers serve every resource type of one provider, and keep nothing: the
// caller keeps the id Create returns, and every other call rebuilds the
// resource's state from it by importing it from the provider.
type handlers struct {
	api       *api
	providers providers
	// timeout bounds a change, which outlives its request.
	timeout time.Duration
	// access, when set, checks every request against the RBAC.
	access *access
}

func newHandlers(a *api, ps providers, timeout time.Duration) *handlers {
	return &handlers{api: a, providers: ps, timeout: timeout}
}

func (h *handlers) routes(r gin.IRouter) {
	for t := range h.api.types {
		base := strings.NewReplacer("{namespace}", ":namespace").Replace(h.api.base(t))
		var guard []gin.HandlerFunc
		if h.access != nil {
			guard = []gin.HandlerFunc{h.authorize(t)}
		}
		r.POST(base, append(guard, h.create(t))...)
		r.GET(base+"/:id", append(guard, h.get(t))...)
		r.PUT(base+"/:id", append(guard, h.update(t))...)
		r.DELETE(base+"/:id", append(guard, h.delete(t))...)
	}
}

func (h *handlers) create(t string) gin.HandlerFunc {
	return func(c *gin.Context) {
		spec, ok := decodeSpec(c)
		if !ok {
			return
		}
		config, err := h.config(t, c.Param("namespace"), spec, nil)
		if err != nil {
			server.AbortError(c, err)
			return
		}
		p, release, ok := h.acquire(c)
		if !ok {
			return
		}
		defer release()
		ctx, cancel := h.change(c)
		defer cancel()
		res, err := p.Create(ctx, t, config)
		if err != nil {
			h.fail(c, err)
			return
		}
		if err := h.kept(ctx, p, t, c.Param("namespace"), res); err != nil {
			server.AbortError(c, err)
			return
		}
		// No one will get the id: the resource would be no one's.
		if c.Request.Context().Err() != nil {
			server.AbortError(c, h.undo(ctx, p, t, res, "the caller left before the "+t+" was created"))
			return
		}
		c.JSON(http.StatusCreated, h.view(t, res))
	}
}

func (h *handlers) get(t string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, release, ok := h.acquire(c)
		if !ok {
			return
		}
		defer release()
		res, ok := h.current(c, p, t)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, h.view(t, res))
	}
}

func (h *handlers) update(t string) gin.HandlerFunc {
	return func(c *gin.Context) {
		spec, ok := decodeSpec(c)
		if !ok {
			return
		}
		p, release, ok := h.acquire(c)
		if !ok {
			return
		}
		defer release()
		prior, ok := h.current(c, p, t)
		if !ok {
			return
		}
		config, err := h.config(t, c.Param("namespace"), spec, prior.Attributes)
		if err != nil {
			server.AbortError(c, err)
			return
		}
		ctx, cancel := h.change(c)
		defer cancel()
		res, err := p.Update(ctx, prior, config)
		if err != nil {
			h.fail(c, err)
			return
		}
		c.JSON(http.StatusOK, h.view(t, res))
	}
}

func (h *handlers) delete(t string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, release, ok := h.acquire(c)
		if !ok {
			return
		}
		defer release()
		prior, ok := h.current(c, p, t)
		if !ok {
			return
		}
		ctx, cancel := h.change(c)
		defer cancel()
		if err := p.Delete(ctx, prior); err != nil {
			h.fail(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// acquire takes a slot at the provider of the path's namespace, waiting while
// all are taken. It answers and returns false when it cannot, such as when
// the caller leaves first: nothing was done.
func (h *handlers) acquire(c *gin.Context) (p provider, release func(), ok bool) {
	p, release, err := h.providers.acquire(c.Request.Context(), c.Param("namespace"))
	if err != nil {
		server.AbortError(c, err)
		return nil, nil, false
	}
	return p, release, true
}

// change is the context of a change to the provider. It is not cancelled
// when the caller leaves: a change cut short may or may not have been made,
// and no one would know which.
func (h *handlers) change(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c.Request.Context()), h.timeout)
}

// current rebuilds the state of the resource the path names from its id
// alone, as `terraform import` does (an id the provider cannot find is a
// 404), then checks it belongs to the namespace: an id is the caller's to send,
// and could name another namespace's resource.
func (h *handlers) current(c *gin.Context, p provider, t string) (tofu.Resource, bool) {
	id := c.Param("id")
	res, err := p.Import(c.Request.Context(), t, id)
	if err != nil {
		h.fail(c, err)
		return tofu.Resource{}, false
	}
	owns, err := h.owns(t, c.Param("namespace"), res.Attributes)
	if err != nil {
		server.AbortError(c, err)
		return tofu.Resource{}, false
	}
	if !owns {
		server.AbortError(c, infraerrors.NotFound("%s %q not found", t, id))
		return tofu.Resource{}, false
	}
	return res, true
}

// kept makes sure a resource just created holds what the rules fixed: a
// provider may store another value than the one it was given without an
// error, such as the default org for an org id it cannot parse. The
// resource is then out of the namespace's reach, possibly in another namespace's:
// it is deleted.
func (h *handlers) kept(ctx context.Context, p provider, t, namespace string, res tofu.Resource) error {
	owns, err := h.owns(t, namespace, res.Attributes)
	if err != nil || owns {
		return err
	}
	return h.undo(ctx, p, t, res, fmt.Sprintf("the provider stored the %s with other values than the rules fix for namespace %q", t, namespace))
}

// undo deletes a resource just created that must not stay, and returns the
// error that says why, and whether it is gone.
func (h *handlers) undo(ctx context.Context, p provider, t string, res tofu.Resource, why string) error {
	id := fmt.Sprint(res.Attributes["id"])
	if err := p.Delete(ctx, res); err != nil {
		logger.L().Error("left a resource behind", logger.String("type", t), logger.String("id", id), logger.String("why", why), logger.Err(err))
		return infraerrors.Upstream(false, "%s; deleting %s %q failed: %v", why, t, id, err)
	}
	logger.L().Warn("deleted a resource just created", logger.String("type", t), logger.String("id", id), logger.String("why", why))
	return infraerrors.Upstream(false, "%s; it was deleted", why)
}

// owns reports whether every attribute a rule fixes has, in state, the
// value the rule gives for this namespace.
func (h *handlers) owns(t, namespace string, state map[string]any) (bool, error) {
	for name, r := range h.api.rules[t] {
		if r.Value == nil {
			continue
		}
		want, err := r.Value.eval(namespace, nil)
		if err != nil {
			return false, fmt.Errorf("%s.%s: value: %w", t, name, err)
		}
		// Compared as text: a rule may give a number to a string attribute.
		if fmt.Sprint(state[name]) != fmt.Sprint(want) {
			return false, nil
		}
	}
	return true, nil
}

// fail answers with a provider's error. pkg/tofu classifies what it can: a
// configuration the provider refuses is a 422, a change that needs replacing
// the resource a 409, an unknown id a 404. The rest is a 502, which the
// first error rule matching the provider's text turns into the HTTP error it
// stands for; one no rule matches stays a 502 upstream_error.
func (h *handlers) fail(c *gin.Context, err error) {
	var e *infraerrors.Error
	if !infraerrors.As(err, &e) || e.Code != http.StatusBadGateway || (e.Slug != "" && e.Slug != upstreamError) {
		server.AbortError(c, err)
		return
	}
	// The cause holds what the provider's process said when it failed to
	// answer at all, such as a refused connection.
	text := e.Message + " " + e.Solution
	if e.Err != nil {
		text += " " + e.Err.Error()
	}
	for _, r := range h.api.errors {
		if r.Match.MatchString(text) {
			mapped := &infraerrors.Error{
				Code: r.Status, Slug: r.Slug, Title: r.Title, Message: e.Message,
				Solution: e.Solution, Origin: e.Origin, Err: err,
			}
			server.AbortError(c, mapped.WithRetryable(r.Retryable))
			return
		}
	}
	if e.Slug == upstreamError {
		server.AbortError(c, err)
		return
	}
	mapped := infraerrors.Upstream(false, "%s", e.Message)
	mapped.Solution, mapped.Origin, mapped.Err = e.Solution, e.Origin, err
	server.AbortError(c, mapped)
}

// upstreamError is the code pkg/tofu gives a provider's failure it cannot
// classify, as infraerrors.Upstream does.
const upstreamError = "upstream_error"

func decodeSpec(c *gin.Context) (map[string]any, bool) {
	var spec map[string]any
	if err := json.NewDecoder(c.Request.Body).Decode(&spec); err != nil {
		server.AbortError(c, infraerrors.Invalid("body: %v", err))
		return nil, false
	}
	return spec, true
}

// config is what the provider gets: the caller's spec, with the rules of
// the resource type applied. A default fills an attribute the spec lacks; on
// an update, the current value wins over the default, so that renaming a
// folder does not change the uid derived from its first name.
func (h *handlers) config(t, namespace string, spec, prior map[string]any) (map[string]any, error) {
	config := maps.Clone(spec)
	for name, r := range h.api.rules[t] {
		switch {
		case r.Value != nil:
			v, err := r.Value.eval(namespace, spec)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: value: %w", t, name, err)
			}
			config[name] = v
		case r.Default != nil && config[name] == nil:
			if v, ok := prior[name]; ok && v != nil && v != "" {
				config[name] = v
				continue
			}
			v, err := r.Default.eval(namespace, spec)
			if err != nil {
				return nil, infraerrors.Invalid("%s: no value, and the default %s cannot be computed: %v", name, r.Default.source, err)
			}
			if err := r.check(v); err != nil {
				return nil, infraerrors.Invalid("%s: derived from %s: %v", name, r.Default.source, err)
			}
			config[name] = v
		}
	}
	return config, nil
}

// view is a resource as the API returns it: the provider's state, id
// included, sensitive values too, without write-only nor hidden attributes.
func (h *handlers) view(t string, res tofu.Resource) map[string]any {
	b := h.api.types[t]
	state := make(map[string]any, len(res.Attributes))
	for k, v := range res.Attributes {
		if a, ok := b.Attributes[k]; (ok && a.WriteOnly) || h.api.rules[t][k].Hidden {
			continue
		}
		state[k] = v
	}
	return state
}
