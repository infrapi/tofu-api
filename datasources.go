package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/zclconf/go-cty/cty"

	infraerrors "github.com/infrapi/lib/pkg/errors"
	"github.com/infrapi/lib/pkg/server"
	"github.com/infrapi/lib/pkg/tofu"
)

// A data source is read with GET on its collection path, its arguments in
// the query: what a query can carry, a string, a number, a bool, or a list
// or set of them, the parameter then repeated. An argument of another type
// is the service's to set, through a rule's value or default, or left out
// with hidden.

// queryable reports whether an argument of type ty can come in the query.
func queryable(ty cty.Type) bool {
	if ty.IsListType() || ty.IsSetType() {
		ty = ty.ElementType()
	}
	return ty == cty.String || ty == cty.Number || ty == cty.Bool
}

// arguments are the data source's attributes the caller may set in the
// query: neither computed only, nor fixed or hidden by a rule. Sorted.
func arguments(b tofu.Block, rs map[string]rule) []string {
	var names []string
	for name, attr := range b.Attributes {
		r := rs[name]
		if attr.Usage == tofu.UsageComputed || r.Value != nil || r.Hidden {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// parseDataRules compiles the rules of the data sources, as parseRules
// does for the resource types, and refuses an argument the query cannot
// carry and no rule sets.
func parseDataRules(f configFile, filename string, sources map[string]tofu.Block) (rules, error) {
	out := rules{}
	for ds, tr := range f.DataSources {
		b := sources[ds]
		out[ds] = map[string]rule{}
		for name, ar := range tr.Attributes {
			where := fmt.Sprintf("%s: data_sources.%s.attributes.%s", filename, ds, name)
			attr, ok := b.Attributes[name]
			if !ok || attr.Usage == tofu.UsageComputed {
				return nil, fmt.Errorf("%s: %s has no argument %q", where, ds, name)
			}
			r, err := compileRule(ar, b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			if r.Hidden && attr.Usage == tofu.UsageRequired && r.Value == nil && r.Default == nil {
				return nil, fmt.Errorf("%s: a required argument can be hidden only with a value or a default", where)
			}
			out[ds][name] = r
		}
		for _, name := range arguments(b, out[ds]) {
			attr := b.Attributes[name]
			if attr.Nested != nil || !queryable(attr.Type) {
				if r := out[ds][name]; r.Default != nil {
					continue
				}
				return nil, fmt.Errorf("%s: data_sources.%s.attributes.%s: an argument of type %s cannot come in the query: set it with a value or a default, or hide it",
					filename, ds, name, attr.Type.FriendlyName())
			}
		}
	}
	return out, nil
}

// read serves GET on a data source's path: its arguments from the query,
// the rules applied, read by the namespace's provider.
func (h *handlers) read(ds string) gin.HandlerFunc {
	b, rs := h.api.dataSources[ds], h.api.dataRules[ds]
	args := arguments(b, rs)
	return func(c *gin.Context) {
		spec, err := queryArguments(c, b, args)
		if err != nil {
			server.AbortError(c, err)
			return
		}
		config, err := applyRules(rs, ds, c.Param("namespace"), spec, nil)
		if err != nil {
			server.AbortError(c, err)
			return
		}
		p, release, ok := h.acquire(c)
		if !ok {
			return
		}
		defer release()
		attrs, err := p.ReadDataSource(c.Request.Context(), ds, config)
		if err != nil {
			h.fail(c, err)
			return
		}
		c.JSON(http.StatusOK, visible(b, rs, attrs))
	}
}

// queryArguments turns the query into the data source's arguments, each to
// its type; a parameter that is no argument is a 400.
func queryArguments(c *gin.Context, b tofu.Block, args []string) (map[string]any, error) {
	query := c.Request.URL.Query()
	allowed := map[string]bool{}
	for _, name := range args {
		allowed[name] = true
	}
	for name := range query {
		if !allowed[name] {
			return nil, infraerrors.Invalid("query: %q is not an argument of this data source", name)
		}
	}
	spec := map[string]any{}
	for _, name := range args {
		values, ok := query[name]
		if !ok {
			continue
		}
		ty := b.Attributes[name].Type
		if ty.IsListType() || ty.IsSetType() {
			list := make([]any, 0, len(values))
			for _, v := range values {
				x, err := queryValue(ty.ElementType(), v)
				if err != nil {
					return nil, infraerrors.Invalid("query: %s: %v", name, err)
				}
				list = append(list, x)
			}
			spec[name] = list
			continue
		}
		if len(values) > 1 {
			return nil, infraerrors.Invalid("query: %s: one value, not %d", name, len(values))
		}
		x, err := queryValue(ty, values[0])
		if err != nil {
			return nil, infraerrors.Invalid("query: %s: %v", name, err)
		}
		spec[name] = x
	}
	return spec, nil
}

// queryValue is v as a value of the primitive type ty, JSON-shaped.
func queryValue(ty cty.Type, v string) (any, error) {
	switch ty {
	case cty.Number:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", v)
		}
		return f, nil
	case cty.Bool:
		x, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%q is not a boolean", v)
		}
		return x, nil
	}
	return v, nil
}

// dataSourcePaths adds the GET of every data source to the contract's paths,
// beside a resource type's POST on the same path, and its schema.
func (a *api) dataSourcePaths(paths, schemas object) {
	names := make([]string, 0, len(a.dataSources))
	for ds := range a.dataSources {
		names = append(names, ds)
	}
	sort.Strings(names)
	for _, ds := range names {
		b, rs := a.dataSources[ds], a.dataRules[ds]
		name := camel(a.segment(ds)) + "Data"
		schemas[name] = stateSchema(b, rs)
		params := []any{}
		for _, arg := range arguments(b, rs) {
			attr := b.Attributes[arg]
			s := typeSchema(attr.Type)
			describe(s, attr)
			if r, ok := rs[arg]; ok {
				r.apply(s)
			}
			p := object{"name": arg, "in": "query", "schema": s}
			if d, ok := s["description"]; ok {
				p["description"] = d
				delete(s, "description")
			}
			if attr.Usage == tofu.UsageRequired && rs[arg].Default == nil {
				p["required"] = true
			}
			if s["type"] == "array" {
				p["style"], p["explode"] = "form", true
			}
			params = append(params, p)
		}
		op := object{
			"operationId": "read" + camel(a.segment(ds)),
			"tags":        []string{a.segment(ds)},
			"summary":     "Read the data source " + ds,
			"description": b.Description,
			"parameters":  params,
			"responses": object{
				"200":     jsonContent("What the data source reads", ref(name)),
				"default": ref("#/components/responses/Problem"),
			},
		}
		path := a.base(ds)
		item, ok := paths[path].(object)
		if !ok {
			item = object{"parameters": []any{ref("#/components/parameters/namespace")}}
			paths[path] = item
		}
		item["get"] = op
	}
}
