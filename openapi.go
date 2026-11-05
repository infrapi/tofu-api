package main

import (
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/infrapi/lib/pkg/tofu"
)

// object is a JSON Schema or OpenAPI node.
type object = map[string]any

// api is the resource types one provider exposes, and the rules the service
// lays over their schemas.
type api struct {
	provider string
	basePath string
	// namespacePattern is the namespace parameter's pattern.
	namespacePattern string
	// bearer: every operation needs a bearer token (auth).
	bearer bool
	types  map[string]tofu.Block
	rules  rules
	errors []errorRule
}

// segment is the path segment of a resource type: example_project → project.
func (a *api) segment(resourceType string) string {
	return strings.TrimPrefix(resourceType, a.provider+"_")
}

func (a *api) base(resourceType string) string {
	return a.basePath + "/" + a.segment(resourceType)
}

// camel turns folder_permission_item into FolderPermissionItem.
func camel(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' })
	for i, p := range parts {
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

// document is the OpenAPI 3.0 contract: per resource type, a collection to
// create in and an item to get, update and delete, keyed by the provider's id.
// The service keeps nothing, so there is nothing to list.
func (a *api) document() object {
	paths := object{}
	schemas := object{
		"Problem": object{
			"type": "object",
			"properties": object{
				"type":      object{"type": "string"},
				"title":     object{"type": "string"},
				"status":    object{"type": "integer"},
				"detail":    object{"type": "string"},
				"code":      object{"type": "string"},
				"retryable": object{"type": "boolean"},
			},
		},
	}

	types := make([]string, 0, len(a.types))
	for t := range a.types {
		types = append(types, t)
	}
	sort.Strings(types)

	for _, t := range types {
		b := a.types[t]
		name := camel(a.segment(t))
		schemas[name+"Spec"] = specSchema(b, a.rules[t], true)
		// The resource as the provider reports it; id is the provider's,
		// and the caller keeps it, every other call needs it.
		state := stateSchema(b, a.rules[t])
		state["required"] = []string{"id"}
		schemas[name] = state
		tag := a.segment(t)
		body := object{
			"required": true,
			"content":  object{"application/json": object{"schema": ref(name + "Spec")}},
		}
		paths[a.base(t)] = object{
			"parameters": []any{ref("#/components/parameters/namespace")},
			"post": object{
				"operationId": "create" + name,
				"tags":        []string{tag},
				"summary":     "Create a " + t,
				"description": b.Description,
				"requestBody": body,
				"responses": object{
					"201":     jsonContent("Created", ref(name)),
					"default": ref("#/components/responses/Problem"),
				},
			},
		}
		paths[a.base(t)+"/{id}"] = object{
			"parameters": []any{ref("#/components/parameters/namespace"), ref("#/components/parameters/id")},
			"get": object{
				"operationId": "get" + name,
				"tags":        []string{tag},
				"summary":     "Read a " + t + " as the provider sees it now",
				"responses": object{
					"200":     jsonContent("The resource", ref(name)),
					"default": ref("#/components/responses/Problem"),
				},
			},
			"put": object{
				"operationId": "update" + name,
				"tags":        []string{tag},
				"summary":     "Update a " + t,
				"requestBody": body,
				"responses": object{
					"200":     jsonContent("Updated", ref(name)),
					"default": ref("#/components/responses/Problem"),
				},
			},
			"delete": object{
				"operationId": "delete" + name,
				"tags":        []string{tag},
				"summary":     "Delete a " + t,
				"responses": object{
					"204":     object{"description": "Deleted"},
					"default": ref("#/components/responses/Problem"),
				},
			},
		}
	}

	doc := object{
		"openapi": "3.0.3",
		"info": object{
			"title":       a.provider + " resources",
			"version":     "v1",
			"description": "Generated from the " + a.provider + " provider's resource schemas.",
		},
		"paths": paths,
		"components": object{
			"schemas": schemas,
			"parameters": object{
				"namespace": object{
					"name": "namespace", "in": "path", "required": true,
					"schema": object{"type": "string", "pattern": a.namespacePattern},
				},
				"id": object{
					"name": "id", "in": "path", "required": true,
					"description": "The id the provider gave the resource, as returned on create.",
					"schema":      object{"type": "string", "minLength": 1},
				},
			},
			"responses": object{
				"Problem": object{
					"description": "An error, as RFC 9457 problem details",
					"content":     object{"application/problem+json": object{"schema": ref("Problem")}},
				},
			},
		},
	}
	if a.bearer {
		components := doc["components"].(object)
		components["securitySchemes"] = object{
			"bearer": object{
				"type": "http", "scheme": "bearer",
				"description": "A token of the authentication service; the RBAC decides what it may do, per namespace and resource type.",
			},
		}
		doc["security"] = []any{object{"bearer": []string{}}}
	}
	return doc
}

func ref(name string) object {
	if !strings.HasPrefix(name, "#") {
		name = "#/components/schemas/" + name
	}
	return object{"$ref": name}
}

func jsonContent(description string, schema object) object {
	return object{
		"description": description,
		"content":     object{"application/json": object{"schema": schema}},
	}
}

// specSchema is what a caller may send: every attribute the configuration
// takes, but not those only the provider sets (computed), nor id, nor those
// a rule fixes. rs are the rules of the resource type; top is false inside
// nested blocks, where no rule applies.
func specSchema(b tofu.Block, rs map[string]rule, top bool) object {
	props := object{}
	var req []string
	for name, attr := range b.Attributes {
		r, ruled := rs[name]
		if attr.Usage == tofu.UsageComputed || (top && name == "id") || (ruled && (r.Value != nil || r.Hidden)) {
			continue
		}
		var s object
		if attr.Nested != nil {
			s = nest(attr.Nested.Nesting, specSchema(attr.Nested.Block, nil, false), 0, 0)
		} else {
			s = typeSchema(attr.Type)
		}
		describe(s, attr)
		if ruled {
			r.apply(s)
		}
		if attr.Sensitive || attr.WriteOnly {
			s["writeOnly"] = true
			if s["type"] == "string" {
				s["format"] = "password"
			}
		}
		props[name] = s
		if attr.Usage == tofu.UsageRequired && (!ruled || r.Default == nil) {
			req = append(req, name)
		}
	}
	for name, nb := range b.Blocks {
		props[name] = nest(nb.Nesting, specSchema(nb.Block, nil, false), nb.MinItems, nb.MaxItems)
		if nb.MinItems > 0 {
			req = append(req, name)
		}
	}
	return closed(props, req)
}

// stateSchema is what the provider reports back: every attribute, sensitive
// and hidden ones left out. rs are the rules of the resource type, nil inside
// nested blocks.
func stateSchema(b tofu.Block, rs map[string]rule) object {
	props := object{}
	for name, attr := range b.Attributes {
		if attr.Sensitive || attr.WriteOnly || rs[name].Hidden {
			continue
		}
		var s object
		if attr.Nested != nil {
			s = nest(attr.Nested.Nesting, stateSchema(attr.Nested.Block, nil), 0, 0)
		} else {
			s = typeSchema(attr.Type)
		}
		describe(s, attr)
		props[name] = s
	}
	for name, nb := range b.Blocks {
		props[name] = nest(nb.Nesting, stateSchema(nb.Block, nil), 0, 0)
	}
	return object{"type": "object", "properties": props}
}

func describe(s object, attr tofu.Attribute) {
	if attr.Description != "" {
		s["description"] = attr.Description
	}
	if attr.Deprecated {
		s["deprecated"] = true
	}
}

func closed(props object, req []string) object {
	s := object{"type": "object", "properties": props, "additionalProperties": false}
	if len(req) > 0 {
		sort.Strings(req)
		s["required"] = req
	}
	return s
}

// nest wraps a block's object schema the way its nesting mode repeats it.
func nest(mode tofu.Nesting, item object, minItems, maxItems int64) object {
	switch mode {
	case tofu.NestingList, tofu.NestingSet:
		s := object{"type": "array", "items": item}
		if mode == tofu.NestingSet {
			s["uniqueItems"] = true
		}
		if minItems > 0 {
			s["minItems"] = minItems
		}
		if maxItems > 0 {
			s["maxItems"] = maxItems
		}
		return s
	case tofu.NestingMap:
		return object{"type": "object", "additionalProperties": item}
	}
	return item // single, group
}

// typeSchema is the JSON Schema of a cty type, as the provider's JSON
// encoding writes its values.
func typeSchema(ty cty.Type) object {
	switch {
	case ty == cty.String:
		return object{"type": "string"}
	case ty == cty.Number:
		return object{"type": "number"}
	case ty == cty.Bool:
		return object{"type": "boolean"}
	case ty.IsListType():
		return object{"type": "array", "items": typeSchema(ty.ElementType())}
	case ty.IsSetType():
		return object{"type": "array", "items": typeSchema(ty.ElementType()), "uniqueItems": true}
	case ty.IsMapType():
		return object{"type": "object", "additionalProperties": typeSchema(ty.ElementType())}
	case ty.IsObjectType():
		props := object{}
		for name, at := range ty.AttributeTypes() {
			props[name] = typeSchema(at)
		}
		return object{"type": "object", "properties": props, "additionalProperties": false}
	case ty.IsTupleType():
		n := len(ty.TupleElementTypes())
		return object{"type": "array", "minItems": n, "maxItems": n}
	}
	return object{} // cty.DynamicPseudoType: any JSON value
}
