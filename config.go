package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// A configuration file is everything one provider needs: which binary to
// run and how to configure it, the resource types to expose, the rules laid
// over their schemas, and which HTTP error a provider's failure stands for.
// Nothing else differs from one provider to the next.
//
//	api:
//	  base_path: /{provider}/v1/namespaces/{namespace}   # as by default
//	  namespace_pattern: '^[a-z0-9][a-z0-9_-]{0,62}$'    # as by default
//	provider:
//	  binary_path: providers/.../terraform-provider-example_v1.2.0
//	  config:
//	    url: https://example.com
//	    token: ${EXAMPLE_TOKEN}               # from the environment
//	resources:
//	  example_project:
//	    attributes:
//	      owner:
//	        value: ns                         # set by the service, not the caller
//	        hidden: true                      # nor shown to the caller
//	      slug:
//	        default: spec.name.replace(" ", "-")
//	        pattern: '^[a-zA-Z0-9_-]{1,40}$'
//	  example_group: {}                       # exposed as the provider has it
//	errors:
//	  - code: already_exists
//	    match: '\(status 409\)'
//	    status: 409
//
// Expressions are CEL (https://cel.dev) with its strings and regex
// extensions; they see the request body as spec and the namespace of the path
// as ns, CEL reserving the word namespace.
type configFile struct {
	API        apiConfig            `yaml:"api"`
	Provider   providerConfig       `yaml:"provider"`
	Namespaces *namespacesConfig    `yaml:"namespaces"`
	Auth       *authConfig          `yaml:"auth"`
	Types       map[string]typeRules `yaml:"resources"`
	DataSources map[string]typeRules `yaml:"data_sources"`
	Errors     []errorRuleSpec      `yaml:"errors"`
}

const (
	defaultBasePath = "/{provider}/v1/namespaces/{namespace}"
	// defaultNamespacePattern is lower case letters, digits, - and _, 63 at
	// most: the namespace may end up in a Vault path or mount, or, through a
	// rule's ns, in an attribute with a format of its own.
	defaultNamespacePattern = `^[a-z0-9][a-z0-9_-]{0,62}$`
)

type apiConfig struct {
	// BasePath is what the API's paths start with, before the resource type:
	// /{provider}/v1/namespaces/{namespace} unless set. {provider} is the
	// provider's name; {namespace}, a whole segment, is required: it picks
	// the provider's credentials.
	BasePath string `yaml:"base_path"`
	// NamespacePattern is the regular expression a namespace matches, in the
	// contract and so for every request: defaultNamespacePattern unless set. It is
	// searched in the namespace, as OpenAPI does: anchor it.
	NamespacePattern string `yaml:"namespace_pattern"`
}

type providerConfig struct {
	// BinaryPath is the provider's plugin binary; or Source and Version.
	BinaryPath string `yaml:"binary_path"`
	// Source, [host/]namespace/type, and Version name a provider installed
	// as terraform init installs it, from Terraform's CLI configuration:
	// from its plugin_cache_dir, or its network_mirror into it.
	Source  string `yaml:"source"`
	Version string `yaml:"version"`
	// Name is {provider} in the API's paths, and the prefix stripped from
	// resource type names; terraform-provider-example_v1.2.0 is example
	// unless set.
	Name string `yaml:"name"`
	// Parallelism is how many requests the provider gets at once; the others
	// wait. 10 unless set, as Terraform's -parallelism.
	Parallelism int `yaml:"parallelism"`
	// Timeout bounds a create, update or delete, which goes on when the
	// caller leaves: 10m unless set.
	Timeout time.Duration `yaml:"timeout"`
	// Config is the provider's configuration block. ${VAR} in its strings is
	// the environment variable VAR, which must be set; $$ is a $.
	Config map[string]any `yaml:"config"`
}

// namespacesConfig gives each namespace a provider process of its own, configured
// with the namespace's secret in Vault laid over provider.config. Without it,
// one provider serves every namespace.
type namespacesConfig struct {
	// Idle stops a namespace's process unused for that long: 10m unless set.
	Idle time.Duration `yaml:"idle"`
	// MaxAge stops a process that old once no request holds it, so that
	// rotated credentials are read again: 1h unless set.
	MaxAge time.Duration `yaml:"max_age"`
	// MaxProviders bounds the processes running at once: 20 unless set.
	MaxProviders int         `yaml:"max_providers"`
	Vault        vaultConfig `yaml:"vault"`
}

// vaultConfig is where the namespaces' secrets are, and the AppRole to read
// them with. ${VAR} in its values is the environment variable VAR.
type vaultConfig struct {
	Address string `yaml:"address"`
	// CACert is a file of the CA that signed Vault's certificate, when the
	// system's do not.
	CACert string `yaml:"ca_cert"`
	// AppRoleMount is where the AppRole auth method is mounted: approle
	// unless set.
	AppRoleMount string `yaml:"approle_mount"`
	RoleID       string `yaml:"role_id"`
	// SecretID, or SecretIDFile, a file holding it, read at each login.
	SecretID     string `yaml:"secret_id"`
	SecretIDFile string `yaml:"secret_id_file"`
	// Mount is where the KV v2 engine is mounted, {namespace} replaced, for
	// an engine per namespace: secret unless set.
	Mount string `yaml:"mount"`
	// Path is the namespace's secret under Mount, {namespace} replaced. Mount
	// or Path must have it: a secret all namespaces share would give them
	// the same credentials.
	Path string `yaml:"path"`
}

type typeRules struct {
	Attributes map[string]attributeRule `yaml:"attributes"`
}

type attributeRule struct {
	Value       *string  `yaml:"value"`
	Default     *string  `yaml:"default"`
	Hidden      bool     `yaml:"hidden"`
	Pattern     string   `yaml:"pattern"`
	MinLength   *int     `yaml:"minLength"`
	MaxLength   *int     `yaml:"maxLength"`
	Enum        []string `yaml:"enum"`
	Description string   `yaml:"description"`
}

type errorRuleSpec struct {
	Code      string `yaml:"code"`
	Match     string `yaml:"match"`
	Status    int    `yaml:"status"`
	Title     string `yaml:"title"`
	Retryable bool   `yaml:"retryable"`
}

func readConfig(path string) (configFile, error) {
	src, err := os.ReadFile(path) // #nosec G304 G703 -- path given on the command line by the operator
	if err != nil {
		return configFile{}, fmt.Errorf("read config: %w", err)
	}
	return parseConfig(src, path)
}

func parseConfig(src []byte, filename string) (configFile, error) {
	var f configFile
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		if strings.Contains(err.Error(), "field types not found") {
			return configFile{}, fmt.Errorf("%s: types: renamed resources: %w", filename, err)
		}
		return configFile{}, fmt.Errorf("%s: %w", filename, err)
	}
	switch p := f.Provider; {
	case p.BinaryPath == "" && p.Source == "":
		return configFile{}, fmt.Errorf("%s: provider.binary_path, or provider.source and provider.version, is required", filename)
	case p.BinaryPath != "" && (p.Source != "" || p.Version != ""):
		return configFile{}, fmt.Errorf("%s: provider.binary_path, or provider.source and provider.version, not both", filename)
	case p.Source != "":
		src, err := parseSource(p.Source)
		if err != nil {
			return configFile{}, fmt.Errorf("%s: provider.%w", filename, err)
		}
		if !providerVersion.MatchString(p.Version) {
			return configFile{}, fmt.Errorf("%s: provider.version %q: want an exact version, such as 1.2.0", filename, p.Version)
		}
		if f.Provider.Name == "" {
			f.Provider.Name = src.Type
		}
	}
	if f.Provider.Name == "" {
		f.Provider.Name = providerName(f.Provider.BinaryPath)
	}
	if f.Provider.Parallelism == 0 {
		f.Provider.Parallelism = 10
	}
	if f.Provider.Timeout == 0 {
		f.Provider.Timeout = 10 * time.Minute
	}
	if f.Provider.Parallelism < 0 || f.Provider.Timeout < 0 {
		return configFile{}, fmt.Errorf("%s: provider.parallelism and provider.timeout must be positive", filename)
	}
	if err := f.API.check(f.Provider.Name); err != nil {
		return configFile{}, fmt.Errorf("%s: api.base_path: %w", filename, err)
	}
	if err := f.API.checkNamespacePattern(); err != nil {
		return configFile{}, fmt.Errorf("%s: api.namespace_pattern: %w", filename, err)
	}
	if f.Auth != nil {
		if err := f.Auth.check(f.API.BasePath); err != nil {
			return configFile{}, fmt.Errorf("%s: auth: %w", filename, err)
		}
	}
	if f.Namespaces != nil {
		if err := f.Namespaces.check(); err != nil {
			return configFile{}, fmt.Errorf("%s: namespaces: %w", filename, err)
		}
	}
	if len(f.Types) == 0 && len(f.DataSources) == 0 {
		return configFile{}, fmt.Errorf("%s: resources, data_sources: name at least one resource type or data source to expose", filename)
	}
	return f, nil
}

// resolved is the provider's configuration block, with ${VAR} replaced;
// lookup gives the environment. Only serving needs it, and its secrets.
func (p providerConfig) resolved(lookup func(string) (string, bool)) (map[string]any, error) {
	config, err := expand(p.Config, lookup)
	if err != nil {
		return nil, fmt.Errorf("provider.config: %w", err)
	}
	m, _ := config.(map[string]any) // expand turns a map into a map
	return m, nil
}

// checkNamespacePattern sets the namespace pattern, and refuses one that
// does not compile: the server compiles the contract's with Go's regexp too.
func (a *apiConfig) checkNamespacePattern() error {
	if a.NamespacePattern == "" {
		a.NamespacePattern = defaultNamespacePattern
	}
	if _, err := regexp.Compile(a.NamespacePattern); err != nil {
		return err
	}
	return nil
}

// check sets the base path, {provider} replaced, and refuses what would not
// route: it reaches gin and the contract as it is.
func (a *apiConfig) check(provider string) error {
	if a.BasePath == "" {
		a.BasePath = defaultBasePath
	}
	p := strings.ReplaceAll(a.BasePath, "{provider}", provider)
	switch {
	case !strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "/":
		return fmt.Errorf("%q must start with / and be clean, without a final /", a.BasePath)
	case p == "/-" || strings.HasPrefix(p, "/-/"):
		return fmt.Errorf("%q: /-/ is for the operational endpoints", a.BasePath)
	}
	namespaces := 0
	for _, segment := range strings.Split(p, "/") {
		if segment == "{namespace}" {
			namespaces++
		} else if strings.ContainsAny(segment, "{}:*") {
			return fmt.Errorf("%q: segment %q: only {provider} and a whole {namespace} are replaced", a.BasePath, segment)
		}
	}
	if namespaces != 1 {
		return fmt.Errorf("%q must have {namespace} once, as a whole segment", a.BasePath)
	}
	a.BasePath = p
	return nil
}

func (t *namespacesConfig) check() error {
	if t.Idle == 0 {
		t.Idle = 10 * time.Minute
	}
	if t.MaxAge == 0 {
		t.MaxAge = time.Hour
	}
	if t.MaxProviders == 0 {
		t.MaxProviders = 20
	}
	if t.Vault.AppRoleMount == "" {
		t.Vault.AppRoleMount = "approle"
	}
	if t.Vault.Mount == "" {
		t.Vault.Mount = "secret"
	}
	switch v := t.Vault; {
	case t.Idle < 0 || t.MaxAge < 0 || t.MaxProviders < 0:
		return errors.New("idle, max_age and max_providers must be positive")
	case v.Address == "" || v.RoleID == "" || v.Path == "":
		return errors.New("vault: address, role_id and path are required")
	case !strings.Contains(v.Mount, "{namespace}") && !strings.Contains(v.Path, "{namespace}"):
		return errors.New("vault: mount or path must name the namespace, as {namespace}")
	case (v.SecretID == "") == (v.SecretIDFile == ""):
		return errors.New("vault: secret_id or secret_id_file, one of them")
	}
	return nil
}

// resolved is the Vault configuration with ${VAR} replaced; lookup gives the
// environment.
func (v vaultConfig) resolved(lookup func(string) (string, bool)) (vaultConfig, error) {
	for _, f := range []struct {
		name  string
		value *string
	}{
		{"address", &v.Address}, {"ca_cert", &v.CACert}, {"role_id", &v.RoleID}, {"secret_id", &v.SecretID},
		{"secret_id_file", &v.SecretIDFile}, {"path", &v.Path},
	} {
		x, err := expand(*f.value, lookup)
		if err != nil {
			return vaultConfig{}, fmt.Errorf("namespaces.vault.%s: %w", f.name, err)
		}
		*f.value, _ = x.(string) // expand turns a string into a string
	}
	return v, nil
}

// typeNames are the resource types to expose, sorted.
// dataSourceNames are the data sources to expose, sorted.
func (f configFile) dataSourceNames() []string {
	names := make([]string, 0, len(f.DataSources))
	for t := range f.DataSources {
		names = append(names, t)
	}
	sort.Strings(names)
	return names
}

func (f configFile) typeNames() []string {
	names := make([]string, 0, len(f.Types))
	for t := range f.Types {
		names = append(names, t)
	}
	sort.Strings(names)
	return names
}

var pluginName = regexp.MustCompile(`^terraform-provider-([a-z0-9-]+)`)

// providerVersion is an exact version, as a provider's releases name them.
var providerVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// providerName is the provider's name, as its binary's name says it:
// terraform-provider-example_v1.2.0 is example.
func providerName(binaryPath string) string {
	if m := pluginName.FindStringSubmatch(filepath.Base(binaryPath)); m != nil {
		return m[1]
	}
	return filepath.Base(binaryPath)
}

// expand replaces ${VAR} in every string of v with the environment's VAR.
func expand(v any, lookup func(string) (string, bool)) (any, error) {
	switch v := v.(type) {
	case string:
		var missing []string
		out := os.Expand(v, func(name string) string {
			if name == "$" {
				return "$"
			}
			val, ok := lookup(name)
			if !ok {
				missing = append(missing, name)
			}
			return val
		})
		if len(missing) > 0 {
			return nil, fmt.Errorf("environment variable %s is not set", missing[0])
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys) // the same variable named first, every time
		for _, k := range keys {
			e := v[k]
			x, err := expand(e, lookup)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = x
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			x, err := expand(e, lookup)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = x
		}
		return out, nil
	}
	return v, nil
}

// authConfig protects the API with the authentication service of
// infrapi/lib's pkg/auth: every request needs a bearer token it resolves
// (POST /oauth2/userinfo), and, unless authorize is false, its RBAC allows
// the request's verb on the resource type in the namespace (pkg/auth's
// AuthorizePath). The operational endpoints
// under /-/ stay open.
type authConfig struct {
	// URL is the authentication service's, which resolves a token and
	// whose RBAC decides.
	URL string `yaml:"url"`
	// CacheTTL keeps a token's identity that long: 1h unless set, a
	// negative value for none. The RBAC's decisions are never kept.
	CacheTTL time.Duration `yaml:"cache_ttl"`
	// Authorize asks the RBAC for every request: true unless set.
	Authorize *bool `yaml:"authorize"`
	// API is the API group the RBAC's rules name: the <group> of a
	// base_path /apis/<group>/... unless set.
	API string `yaml:"api"`
}

// check sets the defaults; basePath is api.base_path, {provider} replaced.
func (a *authConfig) check(basePath string) error {
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("url %q: want the authentication service's http(s) URL", a.URL)
	}
	if a.Authorize == nil {
		a.Authorize = new(bool)
		*a.Authorize = true
	}
	if !*a.Authorize || a.API != "" {
		return nil
	}
	if group, ok := strings.CutPrefix(basePath, "/apis/"); ok {
		group, _, _ = strings.Cut(group, "/")
		if group != "" && group != "{namespace}" {
			a.API = group
			return nil
		}
	}
	return fmt.Errorf("api: required, the API group the RBAC's rules name: api.base_path %q does not start with /apis/<group>/", basePath)
}
