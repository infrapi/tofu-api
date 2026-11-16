package main

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/infrapi/lib/pkg/tofu"
)

// dryRun reports whether the request asks only for the plan, with
// ?dryRun=true: nothing is applied.
func dryRun(c *gin.Context) bool {
	return c.Query("dryRun") == "true"
}

// planView is a dry run's answer: what applying the request would do, the
// changes from the resource as it is now (its drift, for the spec it was
// created with), without what the caller does not see nor ignores.
type planView struct {
	Action  tofu.Action    `json:"action"`
	Changes []changeView   `json:"changes"`
	Current map[string]any `json:"current,omitempty"`
	Planned map[string]any `json:"planned"`
	Unknown []string       `json:"unknown"`
}

type changeView struct {
	Path      string `json:"path"`
	Current   any    `json:"current,omitempty"`
	Planned   any    `json:"planned,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Unknown   bool   `json:"unknown,omitempty"`
}

// plan answers a dry run of t: prior is the resource now, nil for a create.
func (h *handlers) plan(c *gin.Context, p provider, t string, prior *tofu.Resource, config map[string]any) {
	state := tofu.Resource{ResourceType: t}
	if prior != nil {
		state = *prior
	}
	pl, err := p.Plan(c.Request.Context(), state, config)
	if err != nil {
		h.fail(c, err)
		return
	}
	b, rs := h.api.types[t], h.api.rules[t]
	shown := func(path string) bool {
		name := path
		if i := strings.IndexAny(name, ".["); i >= 0 {
			name = name[:i]
		}
		a, ok := b.Attributes[name]
		r := rs[name]
		return (!ok || !a.WriteOnly) && !r.Hidden && !r.IgnoreDrift
	}
	out := planView{Changes: []changeView{}, Unknown: []string{}, Planned: visible(b, rs, pl.Planned)}
	for _, ch := range pl.Changes {
		if shown(ch.Path) {
			out.Changes = append(out.Changes, changeView{Path: ch.Path, Current: ch.Before, Planned: ch.After, Sensitive: ch.Sensitive, Unknown: ch.Unknown})
		}
	}
	for _, path := range pl.Unknown {
		if shown(path) {
			out.Unknown = append(out.Unknown, path)
		}
	}
	out.Action = pl.Action
	if out.Action == tofu.ActionUpdate && len(out.Changes) == 0 {
		out.Action = tofu.ActionNone // what changes is all ignored or hidden
	}
	if prior != nil {
		out.Current = h.view(t, *prior)
	}
	c.JSON(http.StatusOK, out)
}
