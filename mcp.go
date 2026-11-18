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
// writes is true, and the context below.
//
//	mcp:
//	  instructions: |
//	    A namespace is a tenant. Plan before you change a folder.
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
			b, err := os.ReadFile(path) // #nosec G304 -- a path of the operator's configuration
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

// mcpOptions are the MCP server's options: the configured context, after
// what every tofu-api serves: how its tools map to the provider.
func mcpOptions(f configFile, a *api) (mcp.Options, error) {
	if f.MCP == nil {
		return mcp.Options{}, errors.New("no mcp section")
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

	var b strings.Builder
	fmt.Fprintf(&b, "These tools manage the resources of the Terraform provider %q, by namespace: every path has a namespace, and a resource is only reachable in the namespace it was created in.\n", f.Provider.Name)
	fmt.Fprintf(&b, "Resource types: %s.\n", strings.Join(types, ", "))
	if len(sources) > 0 {
		fmt.Fprintf(&b, "Data sources, read only: %s.\n", strings.Join(sources, ", "))
	}
	b.WriteString("Nothing lists the resources of a type: keep the id a create returns; get_ tools read one by its id.\n")
	b.WriteString("plan_ tools are dry runs: they say what a create or an update would change, from the resource as it is now, and apply nothing. Planning an update with the spec a resource was created with shows what changed outside of the API (its drift).\n")
	if f.MCP.Writes {
		b.WriteString("Plan a change and show the plan before you make it.\n")
	} else {
		b.WriteString("This server is read only: you cannot create, change nor delete anything.\n")
	}
	b.WriteString("An error is an RFC 9457 problem: its detail says what went wrong, its solution what to do, retryable whether trying again may work.\n")
	if f.MCP.Instructions != "" {
		b.WriteString("\n" + f.MCP.Instructions)
	}

	opts := mcp.Options{
		Name:         "tofu-api-" + f.Provider.Name,
		Version:      version(),
		Instructions: b.String(),
		Writes:       f.MCP.Writes,
	}
	for _, d := range f.MCP.Documents {
		opts.Documents = append(opts.Documents, mcp.Document{Name: d.Name, Title: d.Title, Description: d.Description, Content: []byte(d.Content)})
	}
	for _, p := range f.MCP.Prompts {
		opts.Prompts = append(opts.Prompts, mcp.Prompt{Name: p.Name, Title: p.Title, Description: p.Description, Text: p.Text})
	}
	return opts, nil
}
