package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/infrapi/lib/pkg/auth"
	infraerrors "github.com/infrapi/lib/pkg/errors"
	"github.com/infrapi/lib/pkg/server"
)

// accessChecker decides whether a token's caller may do a verb on a
// resource type in a namespace: *auth.Client, through the RBAC of the
// authentication service.
type accessChecker interface {
	Authorize(ctx context.Context, token string, req auth.AuthorizeRequest) (allowed bool, reason string, err error)
}

// access is the RBAC check of every request, when auth.authorize is on.
type access struct {
	checker accessChecker
	// api is the API group the RBAC's rules name.
	api string
}

// authorize asks the RBAC whether the request's caller may do what it asks
// on the resource type t in the path's namespace: the verb is the method's,
// get rather than list for a GET, which always names an id. The
// authentication middleware has already refused a request without a valid
// token; the decision is never cached, so an edited role applies to the
// next request.
func (h *handlers) authorize(t string) gin.HandlerFunc {
	resource := h.api.segment(t)
	return func(c *gin.Context) {
		token, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		token = strings.TrimSpace(token)
		if !ok || token == "" {
			server.AbortError(c, infraerrors.New(http.StatusUnauthorized, "tofu-api.authorize", "missing bearer token"))
			return
		}
		verb, err := auth.VerbFor(c.Request.Method, c.Param("id") != "")
		if err != nil {
			server.AbortError(c, infraerrors.New(http.StatusMethodNotAllowed, "tofu-api.authorize", "%s", err))
			return
		}
		allowed, reason, err := h.access.checker.Authorize(c.Request.Context(), token, auth.AuthorizeRequest{
			Namespace: c.Param("namespace"), Api: h.access.api, Resource: resource, Verb: verb,
		})
		if err != nil {
			server.AbortError(c, err)
			return
		}
		if !allowed {
			if reason == "" {
				reason = "not authorized"
			}
			server.AbortError(c, infraerrors.New(http.StatusForbidden, "tofu-api.authorize",
				"%s %s in namespace %q: %s", verb, resource, c.Param("namespace"), reason))
			return
		}
		c.Next()
	}
}
