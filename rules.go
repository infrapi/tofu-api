package main

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/ext"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/infrapi/lib/pkg/tofu"
)

// rule is an attribute's rules, checked against its schema and compiled.
type rule struct {
	// Value fixes the attribute: it leaves the request schema, and a resource
	// whose state does not hold it belongs to another namespace. It may only
	// read ns.
	Value *expr
	// Default is used when the request has no value for the attribute.
	Default *expr
	// Hidden leaves the attribute out of the contract altogether, request
	// and response: what the caller need not know, such as the org.
	Hidden bool

	// Constraints, added to the request schema and checked on a default.
	Pattern     *regexp.Regexp
	MinLength   *int
	MaxLength   *int
	Enum        []string
	Description string
}

// rules are per resource type, then per attribute.
type rules map[string]map[string]rule

// errorRule turns a provider's error into the HTTP error it stands for.
// pkg/tofu classifies what it can; a failure during apply is a 502 with the
// provider's text, and the text is all there is to tell a conflict from an
// outage.
type errorRule struct {
	Slug      string
	Match     *regexp.Regexp
	Status    int
	Title     string
	Retryable bool
}

// expr is a compiled CEL expression, with its text for the contract and for
// error messages.
type expr struct {
	source string
	prog   cel.Program
}

// valueEnv compiles value expressions, which see the namespace only, as ns:
// CEL reserves the word namespace;
// defaultEnv compiles defaults, which also see the request body.
var valueEnv, defaultEnv = mustEnv(false), mustEnv(true)

func mustEnv(withSpec bool) *cel.Env {
	opts := []cel.EnvOption{cel.Variable("ns", cel.StringType), cel.OptionalTypes(), ext.Strings(), ext.Regex()}
	if withSpec {
		opts = append(opts, cel.Variable("spec", cel.MapType(cel.StringType, cel.DynType)))
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		panic(err) // fixed options: only a programming error gets here
	}
	return env
}

// parseRules checks the rules of a configuration file against the schemas
// of the types it exposes: every rule names an attribute the caller could
// otherwise send, and every expression compiles and reads only what it will
// be given.
func parseRules(f configFile, filename string, types map[string]tofu.Block) (rules, []errorRule, error) {
	out := rules{}
	for t, tr := range f.Types {
		b := types[t]
		out[t] = map[string]rule{}
		for name, ar := range tr.Attributes {
			where := fmt.Sprintf("%s: types.%s.attributes.%s", filename, t, name)
			attr, ok := b.Attributes[name]
			if !ok || attr.Usage == tofu.UsageComputed {
				return nil, nil, fmt.Errorf("%s: %s has no attribute %q a caller could set", where, t, name)
			}
			r, err := compileRule(ar, b)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", where, err)
			}
			if r.Hidden && attr.Usage == tofu.UsageRequired && r.Value == nil && r.Default == nil {
				return nil, nil, fmt.Errorf("%s: a required attribute can be hidden only with a value or a default", where)
			}
			out[t][name] = r
		}
	}

	errs := make([]errorRule, 0, len(f.Errors))
	for i, es := range f.Errors {
		er, err := compileErrorRule(es)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: errors[%d] (%s): %w", filename, i, es.Code, err)
		}
		errs = append(errs, er)
	}
	return out, errs, nil
}

func compileRule(ar attributeRule, b tofu.Block) (rule, error) {
	r := rule{
		Hidden:      ar.Hidden,
		MinLength:   ar.MinLength,
		MaxLength:   ar.MaxLength,
		Enum:        ar.Enum,
		Description: ar.Description,
	}
	if ar.Value != nil && ar.Default != nil {
		return rule{}, errors.New("value and default exclude each other")
	}
	var err error
	if ar.Value != nil {
		if r.Value, err = compile(valueEnv, *ar.Value); err != nil {
			return rule{}, fmt.Errorf("value: %w", err)
		}
	}
	if ar.Default != nil {
		if r.Default, err = compile(defaultEnv, *ar.Default); err != nil {
			return rule{}, fmt.Errorf("default: %w", err)
		}
		if err := checkReads(defaultEnv, *ar.Default, b); err != nil {
			return rule{}, fmt.Errorf("default: %w", err)
		}
	}
	if ar.Pattern != "" {
		if r.Pattern, err = regexp.Compile(ar.Pattern); err != nil {
			return rule{}, fmt.Errorf("pattern: %w", err)
		}
	}
	return r, nil
}

func compileErrorRule(es errorRuleSpec) (errorRule, error) {
	if es.Code == "" || es.Match == "" || es.Status < 400 || es.Status > 599 {
		return errorRule{}, errors.New("code, match and a status between 400 and 599 are required")
	}
	match, err := regexp.Compile(es.Match)
	if err != nil {
		return errorRule{}, fmt.Errorf("match: %w", err)
	}
	er := errorRule{Slug: es.Code, Match: match, Status: es.Status, Title: es.Title, Retryable: es.Retryable}
	if er.Title == "" {
		er.Title = http.StatusText(er.Status)
	}
	return er, nil
}

func compile(env *cel.Env, source string) (*expr, error) {
	checked, iss := env.Compile(source)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	prog, err := env.Program(checked)
	if err != nil {
		return nil, err
	}
	return &expr{source: source, prog: prog}, nil
}

// checkReads makes sure a default reads spec only as spec.<attribute>, of
// attributes the caller may send: one it cannot send would never be there.
func checkReads(env *cel.Env, source string, b tofu.Block) error {
	parsed, iss := env.Parse(source)
	if iss.Err() != nil {
		return iss.Err()
	}
	root := ast.NavigateAST(parsed.NativeRep())
	for _, id := range ast.MatchDescendants(root, ast.KindMatcher(ast.IdentKind)) {
		if id.AsIdent() != "spec" {
			continue
		}
		parent, ok := id.Parent()
		if !ok || parent.Kind() != ast.SelectKind {
			return errors.New("read spec.<attribute>, not spec as a whole")
		}
		field := parent.AsSelect().FieldName()
		if a, ok := b.Attributes[field]; !ok || a.Usage == tofu.UsageComputed {
			return fmt.Errorf("reads spec.%s, which the caller cannot send", field)
		}
	}
	return nil
}

// eval evaluates the expression for one request; the result is a plain Go
// value, as decoded from JSON.
func (e *expr) eval(namespace string, spec map[string]any) (any, error) {
	if spec == nil {
		spec = map[string]any{}
	}
	out, _, err := e.prog.Eval(map[string]any{"ns": namespace, "spec": spec})
	if err != nil {
		return nil, err
	}
	native, err := out.ConvertToNative(reflect.TypeFor[*structpb.Value]())
	if err != nil {
		return nil, err
	}
	v, ok := native.(*structpb.Value)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected result %T", e.source, native)
	}
	return v.AsInterface(), nil
}

// check reports how a value breaks the rule's constraints, if it does.
func (r rule) check(v any) error {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	n := utf8.RuneCountInString(s)
	switch {
	case r.Pattern != nil && !r.Pattern.MatchString(s):
		return fmt.Errorf("%q does not match %s", s, r.Pattern)
	case r.MinLength != nil && n < *r.MinLength:
		return fmt.Errorf("%q is shorter than %d", s, *r.MinLength)
	case r.MaxLength != nil && n > *r.MaxLength:
		return fmt.Errorf("%q is longer than %d", s, *r.MaxLength)
	case len(r.Enum) > 0 && !contains(r.Enum, s):
		return fmt.Errorf("%q is not one of %s", s, strings.Join(r.Enum, ", "))
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// apply adds a rule's constraints and documentation to the attribute's
// request schema.
func (r rule) apply(s object) {
	if r.Description != "" {
		s["description"] = r.Description
	}
	if r.Pattern != nil {
		s["pattern"] = r.Pattern.String()
	}
	if r.MinLength != nil {
		s["minLength"] = *r.MinLength
	}
	if r.MaxLength != nil {
		s["maxLength"] = *r.MaxLength
	}
	if len(r.Enum) > 0 {
		enum := append([]string(nil), r.Enum...)
		sort.Strings(enum)
		s["enum"] = enum
	}
	if r.Default != nil {
		s["x-default"] = r.Default.source
		d, _ := s["description"].(string)
		s["description"] = strings.TrimSpace(d + " Defaults to `" + r.Default.source + "`.")
	}
}
