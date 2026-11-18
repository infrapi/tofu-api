package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/infrapi/lib/pkg/tofu"
)

// The documents and prompts tofu-api generates for the MCP server, from the
// provider's schemas and the rules: one document per resource type and data
// source, two prompts per resource type. A configured document or prompt of
// the same name replaces the generated one.

// typeDoc is the document of a resource type: what the tools take, set by
// whom, with which constraints, and a minimal body.
func (a *api) typeDoc(t string, writes bool) string {
	b, rs := a.types[t], a.rules[t]
	seg := a.segment(t)
	var w strings.Builder
	fmt.Fprintf(&w, "# %s\n\n", t)
	if b.Description != "" {
		w.WriteString(strings.TrimSpace(b.Description) + "\n\n")
	}
	fmt.Fprintf(&w, "Path: `%s/{id}`.\n\n", a.base(t))
	tools := []string{"get_" + seg, "plan_create_" + seg, "plan_update_" + seg}
	if writes {
		tools = append(tools, "create_"+seg, "update_"+seg, "delete_"+seg)
	}
	fmt.Fprintf(&w, "Tools: `%s`. A create's and an update's `body` is the spec below; the answer is the resource, with its `id`.\n\n", strings.Join(tools, "`, `"))

	w.WriteString("## Attributes\n\n| Attribute | Type | Set by | Notes |\n|---|---|---|---|\n")
	var fixed, drift []string
	for _, name := range sortedKeys(b.Attributes) {
		attr, r := b.Attributes[name], rs[name]
		switch {
		case r.Hidden:
			continue
		case r.Value != nil:
			fixed = append(fixed, name)
			continue
		}
		if r.IgnoreDrift {
			drift = append(drift, name)
		}
		attrRow(&w, name, attr, r, name == "id")
	}
	for _, name := range sortedKeys(b.Blocks) {
		blockRows(&w, name, b.Blocks[name])
	}
	w.WriteString("\n")
	if len(fixed) > 0 {
		fmt.Fprintf(&w, "Set by the service, from the namespace; not in the body: `%s`.\n\n", strings.Join(fixed, "`, `"))
	}
	if len(drift) > 0 {
		fmt.Fprintf(&w, "Not compared by a plan, the provider cannot read them back: `%s`.\n\n", strings.Join(drift, "`, `"))
	}

	if ex, err := json.MarshalIndent(example(b, rs, true), "", "  "); err == nil {
		fmt.Fprintf(&w, "## Minimal body\n\n```json\n%s\n```\n", ex)
	}
	return w.String()
}

// dataDoc is the document of a data source: its arguments and what it
// answers.
func (a *api) dataDoc(ds string) string {
	b, rs := a.dataSources[ds], a.dataRules[ds]
	var w strings.Builder
	fmt.Fprintf(&w, "# %s (data source)\n\n", ds)
	if b.Description != "" {
		w.WriteString(strings.TrimSpace(b.Description) + "\n\n")
	}
	fmt.Fprintf(&w, "Tool: `read_%s`, read only: it reads what the provider finds, nothing is created.\n\n", a.segment(ds))
	args := arguments(b, rs)
	if len(args) > 0 {
		w.WriteString("## Arguments\n\n| Argument | Type | Notes |\n|---|---|---|\n")
		for _, name := range args {
			attr := b.Attributes[name]
			fmt.Fprintf(&w, "| `%s` | %s | %s |\n", name, typeName(attr), notes(attr, rs[name], attr.Usage == tofu.UsageRequired))
		}
		w.WriteString("\n")
	}
	w.WriteString("## Answer\n\n| Attribute | Type | Notes |\n|---|---|---|\n")
	for _, name := range sortedKeys(b.Attributes) {
		attr, r := b.Attributes[name], rs[name]
		if r.Hidden || r.Value != nil {
			continue
		}
		fmt.Fprintf(&w, "| `%s` | %s | %s |\n", name, typeName(attr), notes(attr, r, false))
	}
	for _, name := range sortedKeys(b.Blocks) {
		fmt.Fprintf(&w, "| `%s` | %s | %s |\n", name, nestingName(b.Blocks[name].Nesting), cell(b.Blocks[name].Description))
	}
	return w.String()
}

// typePrompts are the requests an agent's user can pick for a resource
// type.
func (a *api) typePrompts(t string) []mcpPrompt {
	seg := a.segment(t)
	return []mcpPrompt{
		{
			Name:        "check-drift-" + strings.ReplaceAll(seg, "_", "-"),
			Title:       "Check a " + seg + " for drift",
			Description: "Compare a " + t + " with its spec: what changed outside of the API",
			Text: fmt.Sprintf("Read the %s I name with get_%s, in its namespace. Then plan an update with plan_update_%s, "+
				"with the spec I give you, or else the spec it was created with. Show me each change as before → after, "+
				"and say whether the resource drifted. Change nothing.", t, seg, seg),
		},
		{
			Name:        "plan-create-" + strings.ReplaceAll(seg, "_", "-"),
			Title:       "Plan a new " + seg,
			Description: "Write the spec of a new " + t + " and plan its creation",
			Text: fmt.Sprintf("Read doc://%s, then write the spec of the %s I describe, following its constraints. "+
				"Plan its creation with plan_create_%s in the namespace I name, and show me the spec and the plan. "+
				"Do not create it.", t, t, seg),
		},
	}
}

func attrRow(w *strings.Builder, name string, attr tofu.Attribute, r rule, id bool) {
	by := map[tofu.Usage]string{
		tofu.UsageRequired:         "you, required",
		tofu.UsageOptional:         "you, optional",
		tofu.UsageOptionalComputed: "you, or the provider",
		tofu.UsageComputed:         "the provider",
	}[attr.Usage]
	if id {
		by = "the provider"
	}
	if r.Default != nil && attr.Usage != tofu.UsageComputed {
		by = "you, optional"
	}
	fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n", name, typeName(attr), by, notes(attr, r, false))
	if attr.Nested != nil {
		for _, sub := range sortedKeys(attr.Nested.Attributes) {
			attrRow(w, name+"."+sub, attr.Nested.Attributes[sub], rule{}, false)
		}
	}
}

func blockRows(w *strings.Builder, name string, nb tofu.NestedBlock) {
	by := "you, optional"
	if nb.MinItems > 0 {
		by = "you, required"
	}
	fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n", name, nestingName(nb.Nesting), by, cell(nb.Description))
	for _, sub := range sortedKeys(nb.Attributes) {
		attrRow(w, name+"."+sub, nb.Attributes[sub], rule{}, false)
	}
	for _, sub := range sortedKeys(nb.Blocks) {
		blockRows(w, name+"."+sub, nb.Blocks[sub])
	}
}

// notes are an attribute's description and what the rules add to it.
func notes(attr tofu.Attribute, r rule, required bool) string {
	var n []string
	if required {
		n = append(n, "required")
	}
	if r.Description != "" {
		n = append(n, r.Description)
	} else if attr.Description != "" {
		n = append(n, attr.Description)
	}
	if r.Default != nil {
		n = append(n, "default: `"+r.Default.source+"`")
	}
	if r.Pattern != nil {
		n = append(n, "pattern `"+r.Pattern.String()+"`")
	}
	if r.MinLength != nil {
		n = append(n, fmt.Sprintf("at least %d characters", *r.MinLength))
	}
	if r.MaxLength != nil {
		n = append(n, fmt.Sprintf("at most %d characters", *r.MaxLength))
	}
	if len(r.Enum) > 0 {
		n = append(n, "one of `"+strings.Join(r.Enum, "`, `")+"`")
	}
	if attr.Sensitive {
		n = append(n, "sensitive")
	}
	if attr.WriteOnly {
		n = append(n, "write only: never answered")
	}
	if attr.Deprecated {
		n = append(n, "deprecated")
	}
	return cell(strings.Join(n, "; "))
}

func typeName(attr tofu.Attribute) string {
	if attr.Nested != nil {
		return nestingName(attr.Nested.Nesting)
	}
	if attr.Type == cty.NilType {
		return ""
	}
	return attr.Type.FriendlyName()
}

func nestingName(n tofu.Nesting) string {
	switch n {
	case tofu.NestingList:
		return "list of objects"
	case tofu.NestingSet:
		return "set of objects"
	case tofu.NestingMap:
		return "map of objects"
	default:
		return "object"
	}
}

// cell keeps a text on one table row.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

// example is a minimal body: the required attributes and blocks, with a
// value of their type, an enum's first value.
func example(b tofu.Block, rs map[string]rule, top bool) map[string]any {
	out := map[string]any{}
	for name, attr := range b.Attributes {
		r := rs[name]
		if attr.Usage != tofu.UsageRequired || (top && name == "id") || r.Value != nil || r.Hidden || r.Default != nil {
			continue
		}
		switch {
		case len(r.Enum) > 0:
			out[name] = r.Enum[0]
		case attr.Nested != nil:
			out[name] = repeat(attr.Nested.Nesting, example(attr.Nested.Block, nil, false))
		default:
			out[name] = sample(attr.Type)
		}
	}
	for name, nb := range b.Blocks {
		if nb.MinItems > 0 {
			out[name] = repeat(nb.Nesting, example(nb.Block, nil, false))
		}
	}
	return out
}

func repeat(n tofu.Nesting, v map[string]any) any {
	switch n {
	case tofu.NestingList, tofu.NestingSet:
		return []any{v}
	case tofu.NestingMap:
		return map[string]any{"key": v}
	default:
		return v
	}
}

func sample(ty cty.Type) any {
	switch {
	case ty == cty.String:
		return "example"
	case ty == cty.Number:
		return 1
	case ty == cty.Bool:
		return true
	case ty.IsListType(), ty.IsSetType(), ty.IsTupleType():
		return []any{}
	default:
		return map[string]any{}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
