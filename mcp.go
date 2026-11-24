package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/infrapi/lib/pkg/mcp"
)

// mcpConfig serves the API to AI agents over the Model Context Protocol, at
// <root>/-/mcp: a tool per operation of the contract, read only unless
// writes is true, a document per resource type and data source and two
// prompts per resource type, generated, and the context below. Served
// without an mcp section too; enabled: false turns it off.
//
//	mcp:
//	  enabled: true
//	  instructions: |
//	    A namespace is a team's Grafana organization. Plan before you change a folder.
//	  writes: false                   # true: the agent may create, update and delete
//	  documents:
//	    - name: folders
//	      title: Folders
//	      file: docs/folders.md       # relative to this file
//	  prompts:
//	    - name: check-drift
//	      description: Check a folder against its spec
//	      text: Read the folder, then plan it with the spec I give you.
type mcpConfig struct {
	// Enabled is true when unset.
	Enabled      *bool         `yaml:"enabled"`
	Instructions string        `yaml:"instructions"`
	Writes       bool          `yaml:"writes"`
	Documents    []mcpDocument `yaml:"documents"`
	Prompts      []mcpPrompt   `yaml:"prompts"`
}

type mcpDocument struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	// File holds the document, relative to the configuration file; or
	// Content.
	File    string `yaml:"file"`
	Content string `yaml:"content"`
}

type mcpPrompt struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	// Text is the request; or File, relative to the configuration file.
	Text string `yaml:"text"`
	File string `yaml:"file"`
}

var mcpName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// load checks the names and reads the files, relative to dir.
func (m *mcpConfig) load(dir string) error {
	read := func(kind, name, file string, inline *string) error {
		if !mcpName.MatchString(name) {
			return fmt.Errorf("%s %q: want a name of lower case letters, digits, - and _", kind, name)
		}
		switch {
		case file != "" && *inline != "":
			return fmt.Errorf("%s %s: file or inline text, not both", kind, name)
		case file != "":
			path := file
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			b, err := os.ReadFile(path) // #nosec G304 G703 -- a path of the operator's configuration
			if err != nil {
				return fmt.Errorf("%s %s: %w", kind, name, err)
			}
			*inline = string(b)
		case *inline == "":
			return fmt.Errorf("%s %s: no file nor text", kind, name)
		}
		return nil
	}
	seen := map[string]bool{}
	for i := range m.Documents {
		d := &m.Documents[i]
		if seen["d:"+d.Name] {
			return fmt.Errorf("document %s twice", d.Name)
		}
		seen["d:"+d.Name] = true
		if err := read("document", d.Name, d.File, &d.Content); err != nil {
			return err
		}
	}
	for i := range m.Prompts {
		p := &m.Prompts[i]
		if seen["p:"+p.Name] {
			return fmt.Errorf("prompt %s twice", p.Name)
		}
		seen["p:"+p.Name] = true
		if err := read("prompt", p.Name, p.File, &p.Text); err != nil {
			return err
		}
	}
	return nil
}

// enabled: no section, or one without enabled: false.
func (m *mcpConfig) enabled() bool {
	return m == nil || m.Enabled == nil || *m.Enabled
}

// mcpOptions are the MCP server's options: what tofu-api generates, how its
// tools map to the provider and a document per type, then the configured
// context, whose documents and prompts replace generated ones of the same
// name.
func mcpOptions(f configFile, a *api) (mcp.Options, error) {
	m := f.MCP
	if m == nil {
		m = &mcpConfig{}
	}
	if !m.enabled() {
		return mcp.Options{}, errors.New("mcp is disabled")
	}
	types := make([]string, 0, len(a.types))
	for t := range a.types {
		types = append(types, t)
	}
	sort.Strings(types)
	sources := make([]string, 0, len(a.dataSources))
	for t := range a.dataSources {
		sources = append(sources, t)
	}
	sort.Strings(sources)

	docs := map[string]mcp.Document{}
	for _, t := range types {
		docs[t] = mcp.Document{Name: t, Title: t, Description: "The " + t + " resource type: its attributes, who sets them, their constraints, a minimal body", Content: []byte(a.typeDoc(t, m.Writes))}
	}
	for _, ds := range sources {
		docs[ds] = mcp.Document{Name: ds, Title: ds + " (data source)", Description: "The " + ds + " data source: its arguments and its answer", Content: []byte(a.dataDoc(ds))}
	}
	for _, d := range m.Documents {
		docs[d.Name] = mcp.Document{Name: d.Name, Title: d.Title, Description: d.Description, Content: []byte(d.Content)}
	}
	prompts := map[string]mcp.Prompt{}
	for _, t := range types {
		for _, p := range a.typePrompts(t) {
			prompts[p.Name] = mcp.Prompt{Name: p.Name, Title: p.Title, Description: p.Description, Text: p.Text}
		}
	}
	for _, p := range m.Prompts {
		prompts[p.Name] = mcp.Prompt{Name: p.Name, Title: p.Title, Description: p.Description, Text: p.Text}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "These tools manage the resources of the Terraform provider %q, by namespace: every path has a namespace, and a resource is only reachable in the namespace it was created in.\n", f.Provider.Name)
	fmt.Fprintf(&b, "Resource types: %s.\n", strings.Join(types, ", "))
	if len(sources) > 0 {
		fmt.Fprintf(&b, "Data sources, read only: %s.\n", strings.Join(sources, ", "))
	}
	b.WriteString("Nothing lists the resources of a type: keep the id a create returns; get_ tools read one by its id.\n")
	b.WriteString("plan_ tools are dry runs: they say what a create or an update would change, from the resource as it is now, and apply nothing. Planning an update with the spec a resource was created with shows what changed outside of the API (its drift).\n")
	if m.Writes {
		b.WriteString("Plan a change with its plan_ tool and show the plan before you make it.\n")
	} else {
		b.WriteString("This server is read only: you cannot create, change nor delete anything.\n")
	}
	b.WriteString("An error is an RFC 9457 problem: its detail says what went wrong, its solution what to do, retryable whether trying again may work.\n")
	b.WriteString("\nBefore you write the spec of a resource type, read its document, resource doc://<type> (doc://" + strings.Join(types, ", doc://") + "): its attributes, which are yours to set, their constraints, a minimal body.\n")
	var own []string
	for _, d := range m.Documents {
		own = append(own, fmt.Sprintf("doc://%s (%s)", d.Name, firstOf(d.Title, d.Description, d.Name)))
	}
	if len(own) > 0 {
		b.WriteString("Also read, when it bears on the request: " + strings.Join(own, ", ") + ".\n")
	}
	if m.Instructions != "" {
		b.WriteString("\n" + m.Instructions)
	}

	opts := mcp.Options{
		Name:         "tofu-api-" + f.Provider.Name,
		Version:      version(),
		Instructions: b.String(),
		Writes:       m.Writes,
	}
	for _, name := range sortedKeys(docs) {
		opts.Documents = append(opts.Documents, docs[name])
	}
	for _, name := range sortedKeys(prompts) {
		opts.Prompts = append(opts.Prompts, prompts[name])
	}
	return opts, nil
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
