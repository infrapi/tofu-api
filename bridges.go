package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	infraerrors "github.com/infrapi/lib/pkg/errors"
	"github.com/infrapi/lib/pkg/mcp"
	"github.com/infrapi/lib/pkg/mcp/bridge"
	"github.com/infrapi/lib/pkg/tofu"
)

// bridgeConfig is another MCP server whose tools the API serves as its own
// (mcp.bridges): each one a route of the contract, authorized as a resource
// of the API, and an MCP tool, called with the namespace's own provider
// configuration.
//
//	mcp:
//	  bridges:
//	    - name: grafana
//	      url: http://127.0.0.1:8000/mcp
//	      tools:                            # first match wins; others are not bridged
//	        - {match: "query_prometheus.*|list_prometheus_.*", resource: mcp-prometheus}
//	        - {match: "query_loki.*|list_loki_.*", resource: mcp-loki}
//	      headers:                          # every request; ${VAR} from the environment
//	        Authorization: Bearer ${MCP_GRAFANA_TOKEN}
//	      call_headers:                     # each call: a key of the namespace's configuration
//	        X-Grafana-Service-Account-Token: auth
//	        X-Grafana-Org-Id: grafana_org_id  # only bridges read it: not passed to the provider
type bridgeConfig struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// ProtocolVersion is the MCP protocol version of the session with it:
	// bridge.DefaultProtocolVersion unless set.
	ProtocolVersion string       `yaml:"protocol_version"`
	Tools           []bridgeRule `yaml:"tools"`
	// Writes bridges the tools that are not read-only too.
	Writes bool `yaml:"writes"`
	// Headers are sent on every request, their values with ${VAR} replaced.
	Headers map[string]string `yaml:"headers"`
	// CallHeaders are sent with each call: header name -> a key of the
	// namespace's provider configuration (provider.config with the
	// namespace's secret over it).
	CallHeaders map[string]string `yaml:"call_headers"`
	// Timeout bounds one call: bridge.DefaultTimeout unless set.
	Timeout time.Duration `yaml:"timeout"`
}

type bridgeRule struct {
	Match    string `yaml:"match"`
	Resource string `yaml:"resource"`
}

// checkBridges refuses what would not serve: a bridge without a name, an
// url or a rule, a name twice, a resource not named mcp-<family>. A
// resource the contract has already is refused at start, by the bridge.
func (f configFile) checkBridges() error {
	if f.MCP == nil {
		return nil
	}
	names := map[string]bool{}
	for i, b := range f.MCP.Bridges {
		switch {
		case !mcpName.MatchString(b.Name):
			return fmt.Errorf("bridges[%d]: name %q: want lower case letters, digits, - and _", i, b.Name)
		case names[b.Name]:
			return fmt.Errorf("bridges: %s twice", b.Name)
		case b.URL == "":
			return fmt.Errorf("bridges: %s: url is required", b.Name)
		case len(b.Tools) == 0:
			return fmt.Errorf("bridges: %s: tools: no rule selects a tool", b.Name)
		case b.Timeout < 0:
			return fmt.Errorf("bridges: %s: timeout must be positive", b.Name)
		}
		names[b.Name] = true
		for _, r := range b.Tools {
			if !strings.HasPrefix(r.Resource, bridge.ResourcePrefix) {
				return fmt.Errorf("bridges: %s: resource %q: want %s<family>, as %sprometheus", b.Name, r.Resource, bridge.ResourcePrefix, bridge.ResourcePrefix)
			}
		}
		for h, key := range b.CallHeaders {
			if key == "" {
				return fmt.Errorf("bridges: %s: call_headers: %s: name a key of the provider's configuration", b.Name, h)
			}
		}
	}
	return nil
}

// bridgeKeys are the keys of the namespace's configuration a bridge sends
// (call_headers).
func (f configFile) bridgeKeys() map[string]bool {
	keys := map[string]bool{}
	if f.MCP == nil {
		return keys
	}
	for _, b := range f.MCP.Bridges {
		for _, key := range b.CallHeaders {
			keys[key] = true
		}
	}
	return keys
}

// providerOnly is config without the keys only bridges read: those of
// bridgeKeys the provider's configuration block does not have. A namespace's
// secret may hold what a bridged server needs and the provider refuses
// (Grafana's org id beside a service account token). Any other key is left
// for the provider to refuse: a typo stays an error.
func providerOnly(config map[string]any, schema tofu.Block, bridgeKeys map[string]bool) map[string]any {
	out := make(map[string]any, len(config))
	for k, v := range config {
		_, attr := schema.Attributes[k]
		_, block := schema.Blocks[k]
		if bridgeKeys[k] && !attr && !block {
			continue
		}
		out[k] = v
	}
	return out
}

// bridgeCredentialTTL is how long a namespace's provider configuration read
// for the bridges is kept: a rotated credential applies within it.
const bridgeCredentialTTL = time.Minute

// namespaceConfigs gives a namespace's provider configuration, as its
// provider process is configured with: provider.config, and with namespaces
// the namespace's secret over it, kept bridgeCredentialTTL.
type namespaceConfigs struct {
	base   map[string]any
	secret func(ctx context.Context, namespace string) (map[string]any, error)
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cachedConfig
}

type cachedConfig struct {
	config map[string]any
	until  time.Time
}

func (n *namespaceConfigs) of(ctx context.Context, namespace string) (map[string]any, error) {
	if n.secret == nil {
		return n.base, nil
	}
	n.mu.Lock()
	c, ok := n.cache[namespace]
	n.mu.Unlock()
	if ok && n.now().Before(c.until) {
		return c.config, nil
	}
	secret, err := n.secret(ctx, namespace)
	if err != nil {
		return nil, err
	}
	config := maps.Clone(n.base)
	if config == nil {
		config = map[string]any{}
	}
	maps.Copy(config, secret)
	n.mu.Lock()
	if n.cache == nil {
		n.cache = map[string]cachedConfig{}
	}
	n.cache[namespace] = cachedConfig{config: config, until: n.now().Add(n.ttl)}
	n.mu.Unlock()
	return config, nil
}

// bridges are the bridged MCP servers of the configuration.
type bridges struct {
	list []*bridge.Bridge
	// base is the API's namespaced path.
	base string
}

// startBridges connects to each bridged server and lists its tools.
func startBridges(ctx context.Context, f configFile, configs *namespaceConfigs, lookup func(string) (string, bool)) (*bridges, error) {
	out := &bridges{base: f.API.BasePath}
	if f.MCP == nil {
		return out, nil
	}
	for _, bc := range f.MCP.Bridges {
		headers := http.Header{}
		for h, v := range bc.Headers {
			x, err := expand(v, lookup)
			if err != nil {
				return nil, errors.Join(fmt.Errorf("mcp.bridges: %s: headers: %s: %w", bc.Name, h, err), out.close())
			}
			headers.Set(h, x.(string)) // expand turns a string into a string
		}
		rules := make([]bridge.Rule, 0, len(bc.Tools))
		for _, r := range bc.Tools {
			rules = append(rules, bridge.Rule{Match: r.Match, Resource: r.Resource})
		}
		b, err := bridge.New(ctx, bridge.Config{
			Name: bc.Name, URL: bc.URL, ProtocolVersion: bc.ProtocolVersion, Rules: rules, Writes: bc.Writes,
			Headers: headers, CallHeaders: callHeaders(bc, configs), Timeout: bc.Timeout,
			NamespacePattern: f.API.NamespacePattern,
		})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("mcp.bridges: %w", err), out.close())
		}
		out.list = append(out.list, b)
	}
	return out, nil
}

// callHeaders are a call's headers, from the namespace's provider
// configuration.
func callHeaders(bc bridgeConfig, configs *namespaceConfigs) func(*http.Request, string) (http.Header, error) {
	if len(bc.CallHeaders) == 0 {
		return nil
	}
	return func(r *http.Request, namespace string) (http.Header, error) {
		config, err := configs.of(r.Context(), namespace)
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		for name, key := range bc.CallHeaders {
			v, ok := config[key]
			if !ok || v == nil || v == "" {
				return nil, infraerrors.UpstreamAuth("namespace %q: its provider configuration has no %s, which the bridge %s sends as %s", namespace, key, bc.Name, name).
					WithSolution("set %s in the namespace's secret, or in provider.config", key)
			}
			h.Set(name, fmt.Sprint(v))
		}
		return h, nil
	}
}

// contract is contract with every bridge's routes.
func (bs *bridges) contract(contract []byte) ([]byte, error) {
	for _, b := range bs.list {
		var err error
		if contract, err = b.Contract(contract, bs.base); err != nil {
			return nil, err
		}
	}
	return contract, nil
}

// routes registers the bridges' routes, each behind the RBAC check of its
// resource when auth.authorize is on.
func (bs *bridges) routes(r gin.IRoutes, h *handlers) error {
	var guard func(string) []gin.HandlerFunc
	if h.access != nil {
		guard = func(resource string) []gin.HandlerFunc {
			// a route names a tool: an item, get for a read, create for a write
			return []gin.HandlerFunc{h.authorizeResource(resource, func(*gin.Context) bool { return true })}
		}
	}
	for _, b := range bs.list {
		if err := b.Register(r, bs.base, guard); err != nil {
			return err
		}
	}
	return nil
}

// tools are the bridged tools, for the MCP server.
func (bs *bridges) tools() []mcp.Tool {
	var out []mcp.Tool
	for _, b := range bs.list {
		out = append(out, b.Tools(bs.base)...)
	}
	return out
}

// describe tells the agent which resources each bridge's tools are of.
func (bs *bridges) describe(f configFile) string {
	if f.MCP == nil || len(bs.list) == 0 {
		return ""
	}
	var lines []string
	for i, b := range bs.list {
		lines = append(lines, fmt.Sprintf("Tools of %s (%s): each takes the namespace whose %s it reaches.",
			f.MCP.Bridges[i].Name, strings.Join(b.Resources(), ", "), f.Provider.Name))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

func (bs *bridges) close() error {
	var errs []error
	for _, b := range bs.list {
		errs = append(errs, b.Close())
	}
	return errors.Join(errs...)
}
