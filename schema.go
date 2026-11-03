package main

import (
	"fmt"
	"net/http"
	"sort"

	infraerrors "github.com/infrapi/lib/pkg/errors"
	"github.com/infrapi/lib/pkg/tofu"
)

// schemaSource is what loadSchemas needs from a running provider;
// *tofu.Provider has it.
type schemaSource interface {
	ResourceTypes() []string
	ResourceSchema(resourceType string) (tofu.Block, error)
}

// loadSchemas reads the schemas of the given managed resource types from a
// running provider.
func loadSchemas(p schemaSource, types []string) (map[string]tofu.Block, error) {
	out := map[string]tofu.Block{}
	for _, name := range types {
		b, err := p.ResourceSchema(name)
		if infraerrors.ErrorCode(err) == http.StatusNotFound {
			known := p.ResourceTypes()
			sort.Strings(known)
			return nil, fmt.Errorf("provider has no resource type %q (it has %d, e.g. %v)", name, len(known), known[:min(5, len(known))])
		}
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	return out, nil
}
