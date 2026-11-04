package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl"
)

// Providers named by source and version are installed as `terraform init`
// installs them, from the CLI configuration Terraform reads (the file
// TF_CLI_CONFIG_FILE names, ~/.terraformrc otherwise):
//
//	plugin_cache_dir = "/var/lib/tofu-api/providers"
//	provider_installation {
//	  network_mirror {
//	    url = "https://artifactory.example.com/artifactory/api/terraform/terraform-providers/providers/"
//	  }
//	}
//	credentials "artifactory.example.com" {
//	  token = "..."
//	}
//
// A version already in plugin_cache_dir, laid out as Terraform lays it out,
// is run as it is; another is downloaded from the network mirror, checked
// against the hashes the mirror gives, and unpacked there for the next start.

// defaultRegistry is the host of a source that names none.
const defaultRegistry = "registry.terraform.io"

// maxArchive bounds a provider's zip, and what it unpacks to.
const maxArchive = 1 << 30

var (
	sourcePart = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	sourceHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
)

// providerSource is a provider's address: [host/]namespace/type.
type providerSource struct {
	Host, Namespace, Type string
}

func parseSource(s string) (providerSource, error) {
	parts := strings.Split(strings.ToLower(s), "/")
	if len(parts) == 2 {
		parts = append([]string{defaultRegistry}, parts...)
	}
	if len(parts) != 3 {
		return providerSource{}, fmt.Errorf("source %q: want [host/]namespace/type", s)
	}
	if !sourceHost.MatchString(parts[0]) || strings.Contains(parts[0], "..") {
		return providerSource{}, fmt.Errorf("source %q: invalid host %q", s, parts[0])
	}
	for _, p := range parts[1:] {
		if !sourcePart.MatchString(p) {
			return providerSource{}, fmt.Errorf("source %q: invalid namespace or type %q", s, p)
		}
	}
	return providerSource{Host: parts[0], Namespace: parts[1], Type: parts[2]}, nil
}

func (s providerSource) String() string { return s.Host + "/" + s.Namespace + "/" + s.Type }

// cliConfig is what tofu-api reads of Terraform's CLI configuration.
type cliConfig struct {
	PluginCacheDir string
	MirrorURL      *url.URL
	Tokens         map[string]string // by host
}

// loadCLIConfig reads the CLI configuration the environment points to, as
// Terraform does: TF_CLI_CONFIG_FILE, or ~/.terraformrc; TF_PLUGIN_CACHE_DIR
// and TF_TOKEN_<host> over what it holds.
func loadCLIConfig(lookup func(string) (string, bool)) (cliConfig, error) {
	path, ok := lookup("TF_CLI_CONFIG_FILE")
	if !ok || path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return cliConfig{}, fmt.Errorf("terraform CLI configuration: TF_CLI_CONFIG_FILE unset and no home directory: %w", err)
		}
		path = filepath.Join(home, ".terraformrc")
	}
	src, err := os.ReadFile(path) // #nosec G304 -- the operator's CLI configuration
	if err != nil {
		return cliConfig{}, fmt.Errorf("terraform CLI configuration: %w", err)
	}
	c, err := parseCLIConfig(string(src))
	if err != nil {
		return cliConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	if dir, ok := lookup("TF_PLUGIN_CACHE_DIR"); ok && dir != "" {
		c.PluginCacheDir = dir
	}
	if c.PluginCacheDir == "" {
		return cliConfig{}, fmt.Errorf("%s: plugin_cache_dir is required: where the providers are installed", path)
	}
	if c.MirrorURL != nil {
		if token, ok := lookup(tokenVariable(c.MirrorURL.Hostname())); ok && token != "" {
			c.Tokens[c.MirrorURL.Hostname()] = token
		}
	}
	return c, nil
}

// tokenVariable is the variable Terraform reads a host's token from:
// TF_TOKEN_ and the host, dots as _ and dashes as __.
func tokenVariable(host string) string {
	return "TF_TOKEN_" + strings.NewReplacer(".", "_", "-", "__").Replace(host)
}

func parseCLIConfig(src string) (cliConfig, error) {
	var raw map[string]any
	if err := hcl.Decode(&raw, src); err != nil {
		return cliConfig{}, err
	}
	c := cliConfig{Tokens: map[string]string{}}
	for key, v := range raw {
		switch key {
		case "plugin_cache_dir":
			s, ok := v.(string)
			if !ok {
				return cliConfig{}, errors.New("plugin_cache_dir: want a string")
			}
			c.PluginCacheDir = s
		case "credentials":
			for _, block := range blocks(v) {
				for host, body := range block {
					for _, attrs := range blocks(body) {
						if token, ok := attrs["token"].(string); ok {
							c.Tokens[strings.ToLower(host)] = token
						}
					}
				}
			}
		case "provider_installation":
			for _, block := range blocks(v) {
				for method, body := range block {
					if method != "network_mirror" {
						return cliConfig{}, fmt.Errorf("provider_installation: %s: tofu-api installs from a network_mirror only", method)
					}
					for _, attrs := range blocks(body) {
						s, _ := attrs["url"].(string)
						if c.MirrorURL != nil {
							return cliConfig{}, errors.New("provider_installation: one network_mirror only")
						}
						u, err := url.Parse(s)
						if err != nil || u.Scheme != "https" || u.Host == "" {
							return cliConfig{}, fmt.Errorf("network_mirror: url %q: want an https URL", s)
						}
						if !strings.HasSuffix(u.Path, "/") {
							u.Path += "/"
						}
						c.MirrorURL = u
					}
				}
			}
		}
		// Anything else is Terraform's, and none of tofu-api's business.
	}
	return c, nil
}

// blocks is an HCL block as hcl.Decode gives it into an any: a list of
// maps, one per occurrence.
func blocks(v any) []map[string]any {
	var out []map[string]any
	switch v := v.(type) {
	case []map[string]any:
		out = v
	case map[string]any:
		out = []map[string]any{v}
	case []any:
		for _, e := range v {
			out = append(out, blocks(e)...)
		}
	}
	return out
}

// installProvider returns the binary of the provider p names by source and
// version, installed as Terraform's CLI configuration says.
func installProvider(ctx context.Context, p providerConfig) (string, error) {
	src, err := parseSource(p.Source)
	if err != nil {
		return "", err
	}
	cfg, err := loadCLIConfig(os.LookupEnv)
	if err != nil {
		return "", err
	}
	return newInstaller(cfg).install(ctx, src, p.Version)
}

// installer installs providers into the plugin cache.
type installer struct {
	cfg    cliConfig
	client *http.Client
}

func newInstaller(cfg cliConfig) *installer {
	return &installer{cfg: cfg, client: &http.Client{Timeout: 10 * time.Minute}}
}

// install returns the binary of the provider's version for this platform,
// from the plugin cache, downloading it there first when it is not.
func (in *installer) install(ctx context.Context, src providerSource, version string) (string, error) {
	platform := runtime.GOOS + "_" + runtime.GOARCH
	dir := filepath.Join(in.cfg.PluginCacheDir, src.Host, src.Namespace, src.Type, version, platform)
	if bin, err := findBinary(dir, src.Type); err == nil {
		return bin, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if in.cfg.MirrorURL == nil {
		return "", fmt.Errorf("provider %s %s is not in %s, and no network_mirror to download it from", src, version, in.cfg.PluginCacheDir)
	}
	if err := in.download(ctx, src, version, platform, dir); err != nil {
		return "", fmt.Errorf("provider %s %s: %w", src, version, err)
	}
	return findBinary(dir, src.Type)
}

// mirrorVersion is a version's document in the network mirror protocol.
type mirrorVersion struct {
	Archives map[string]struct {
		URL    string   `json:"url"`
		Hashes []string `json:"hashes"`
	} `json:"archives"`
}

func (in *installer) download(ctx context.Context, src providerSource, version, platform, dir string) (err error) {
	// #nosec G125 -- parseSource and providerVersion leave no separator nor .. in them
	versionURL := in.cfg.MirrorURL.JoinPath(src.Host, src.Namespace, src.Type, version+".json")
	body, err := in.get(ctx, versionURL)
	if err != nil {
		return err
	}
	var doc mirrorVersion
	err = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&doc)
	err = errors.Join(err, body.Close())
	if err != nil {
		return fmt.Errorf("%s: %w", versionURL.Redacted(), err)
	}
	archive, ok := doc.Archives[platform]
	if !ok {
		return fmt.Errorf("%s: no archive for %s", versionURL.Redacted(), platform)
	}
	archiveURL, err := versionURL.Parse(archive.URL)
	if err != nil {
		return fmt.Errorf("%s: archive url %q: %w", versionURL.Redacted(), archive.URL, err)
	}

	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil { // #nosec G301 -- the providers are run by the service's user
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+"-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()

	zipPath := tmp + ".zip"
	if err := in.save(ctx, archiveURL, zipPath); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Remove(zipPath)) }()
	zh, err := fileSHA256(zipPath)
	if err != nil {
		return err
	}
	if err := unzip(zipPath, tmp); err != nil {
		return fmt.Errorf("%s: %w", archiveURL.Redacted(), err)
	}
	h1, err := hashDir(tmp)
	if err != nil {
		return err
	}
	if err := checkHashes(archive.Hashes, "zh:"+zh, h1); err != nil {
		return fmt.Errorf("%s: %w", archiveURL.Redacted(), err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil { // #nosec G302 -- a directory of executables
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another process installed it meanwhile: theirs is as good.
		if _, statErr := os.Stat(dir); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// checkHashes accepts the archive when one of the hashes the mirror gives is
// the zip's (zh:) or its contents' (h1:), as terraform init does; a mirror
// that gives none is trusted, over HTTPS, as terraform init trusts it.
func checkHashes(want []string, zh, h1 string) error {
	if len(want) == 0 {
		return nil
	}
	for _, w := range want {
		if w == zh || w == h1 {
			return nil
		}
	}
	return fmt.Errorf("checksum mismatch: got %s and %s, the mirror gives %s", zh, h1, strings.Join(want, ", "))
}

// get requests u with the token of its host, if any.
func (in *installer) get(ctx context.Context, u *url.URL) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if token, ok := in.cfg.Tokens[strings.ToLower(u.Hostname())]; ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := in.client.Do(req) // #nosec G704 -- the mirror the operator configured
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("%s: %s: %s", u.Redacted(), resp.Status, strings.TrimSpace(string(msg)))
		return nil, errors.Join(err, readErr, resp.Body.Close())
	}
	return resp.Body, nil
}

func (in *installer) save(ctx context.Context, u *url.URL, path string) (err error) {
	body, err := in.get(ctx, u)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, body.Close()) }()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- under the plugin cache
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(body, maxArchive+1))
	err = errors.Join(err, f.Close())
	if err == nil && n > maxArchive {
		err = fmt.Errorf("%s: archive over %d bytes", u.Redacted(), maxArchive)
	}
	return err
}

func fileSHA256(path string) (_ string, err error) {
	f, err := os.Open(path) // #nosec G304 -- under the plugin cache
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unzip unpacks the regular files of the zip at path into dir, executable,
// refusing any that would land outside it.
func unzip(path, dir string) (err error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	var total uint64
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("%s: not a regular file", f.Name)
		}
		name := filepath.FromSlash(f.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%s: outside the archive", f.Name)
		}
		if total += f.UncompressedSize64; total > maxArchive {
			return fmt.Errorf("unpacks to over %d bytes", maxArchive)
		}
		if err := unzipFile(f, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func unzipFile(f *zip.File, path string) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- read by the service's user
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rc.Close()) }()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755) // #nosec G302 G304 -- a provider's executable, under the plugin cache
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(rc, maxArchive)) // #nosec G110 -- bounded, and by the total in unzip
	return errors.Join(err, out.Close())
}

// hashDir is Terraform's h1: hash of an unpacked provider (dirhash's Hash1):
// the base64 SHA-256 of the lines "<sha256 hex>  <path>", sorted by path.
func hashDir(dir string) (string, error) {
	var lines []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s  %s\n", sum, filepath.ToSlash(rel)))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i][66:] < lines[j][66:] })
	h := sha256.New()
	for _, l := range lines {
		if _, err := io.WriteString(h, l); err != nil {
			return "", err
		}
	}
	return "h1:" + base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// findBinary is the one terraform-provider-<type> file in dir, with or
// without its _v<version>.
func findBinary(dir, typ string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && (n == "terraform-provider-"+typ || strings.HasPrefix(n, "terraform-provider-"+typ+"_")) {
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%s: want one terraform-provider-%s binary, found %d", dir, typ, len(found))
	}
	return filepath.Join(dir, found[0]), nil
}
