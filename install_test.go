package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashDir is dirhash.HashDir(dir, "", dirhash.Hash1), which terraform init
// records as h1: (value computed with golang.org/x/mod/sumdb/dirhash).
func TestHashDirIsTerraformsH1(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o750))
	for name, content := range map[string]string{
		"terraform-provider-thing_v1.2.0": "bin\n",
		"LICENSE":                         "MIT\n",
		"sub/a.txt":                       "x",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	h1, err := hashDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "h1:QdsSkFftwRzfL3qpU6uZNRfMjy98InMD1gpYkf3iJGU=", h1)
}

func TestParseSource(t *testing.T) {
	s, err := parseSource("grafana/grafana")
	require.NoError(t, err)
	assert.Equal(t, providerSource{"registry.terraform.io", "grafana", "grafana"}, s)

	s, err = parseSource("mirror.example.com/Acme/widget")
	require.NoError(t, err)
	assert.Equal(t, providerSource{"mirror.example.com", "acme", "widget"}, s)

	for _, bad := range []string{"grafana", "a/b/c/d", "grafana/../x", "/grafana/grafana", "h@st/a/b", "../a/b", "a..b/c/d", ".x/a/b"} {
		_, err := parseSource(bad)
		assert.Error(t, err, bad)
	}
}

func TestParseCLIConfig(t *testing.T) {
	c, err := parseCLIConfig(`
plugin_cache_dir = "/var/lib/tofu-api/providers"
disable_checkpoint = true
provider_installation {
  network_mirror {
    url = "https://artifactory.example.com/artifactory/api/terraform/terraform-providers/providers"
  }
}
credentials "Artifactory.example.com" {
  token = "secret"
}
credentials "other.example.com" {
  token = "other"
}
`)
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/tofu-api/providers", c.PluginCacheDir)
	assert.Equal(t, "https://artifactory.example.com/artifactory/api/terraform/terraform-providers/providers/", c.MirrorURL.String())
	assert.Equal(t, map[string]string{"artifactory.example.com": "secret", "other.example.com": "other"}, c.Tokens)

	_, err = parseCLIConfig(`provider_installation {
  direct {}
}`)
	assert.ErrorContains(t, err, "network_mirror only")

	_, err = parseCLIConfig(`provider_installation {
  network_mirror { url = "http://mirror.example.com/" }
}`)
	assert.ErrorContains(t, err, "https")
}

func TestLoadCLIConfigFromTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terraformrc")
	require.NoError(t, os.WriteFile(path, []byte(`
plugin_cache_dir = "/from/file"
provider_installation {
  network_mirror { url = "https://my-mirror.example.com/providers/" }
}
`), 0o600))
	env := map[string]string{
		"TF_CLI_CONFIG_FILE":              path,
		"TF_PLUGIN_CACHE_DIR":             "/from/env",
		"TF_TOKEN_my__mirror_example_com": "from-env",
		"TF_TOKEN_unrelated_example_com":  "ignored",
	}
	c, err := loadCLIConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	assert.Equal(t, "/from/env", c.PluginCacheDir)
	assert.Equal(t, map[string]string{"my-mirror.example.com": "from-env"}, c.Tokens)

	require.NoError(t, os.WriteFile(path, []byte(`credentials "x" { token = "y" }`), 0o600))
	delete(env, "TF_PLUGIN_CACHE_DIR")
	_, err = loadCLIConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	assert.ErrorContains(t, err, "plugin_cache_dir is required")
}

// mirror serves one provider version by the network mirror protocol.
type mirror struct {
	zip      []byte
	hashes   []string
	requests atomic.Int32
	auth     atomic.Value // the last Authorization header
}

func (m *mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.requests.Add(1)
	m.auth.Store(r.Header.Get("Authorization"))
	switch r.URL.Path {
	case "/providers/registry.terraform.io/example/thing/1.2.0.json":
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"archives": map[string]any{
			runtime.GOOS + "_" + runtime.GOARCH: map[string]any{
				"url":    "thing/terraform-provider-thing_1.2.0.zip",
				"hashes": m.hashes,
			},
		}}); err != nil {
			panic(err)
		}
	case "/providers/registry.terraform.io/example/thing/thing/terraform-provider-thing_1.2.0.zip":
		if _, err := w.Write(m.zip); err != nil {
			panic(err)
		}
	default:
		http.NotFound(w, r)
	}
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func testInstaller(t *testing.T, m *mirror) (*installer, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(m)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL + "/providers/")
	require.NoError(t, err)
	in := newInstaller(cliConfig{
		PluginCacheDir: t.TempDir(),
		MirrorURL:      u,
		Tokens:         map[string]string{u.Hostname(): "secret"},
	})
	in.client = srv.Client()
	return in, srv
}

var thing = providerSource{"registry.terraform.io", "example", "thing"}

func TestInstallDownloadsOnceAndChecksTheHash(t *testing.T) {
	archive := zipOf(t, map[string]string{"terraform-provider-thing_v1.2.0": "#!/bin/sh\n", "LICENSE": "MIT\n"})
	sum := sha256.Sum256(archive)
	m := &mirror{zip: archive, hashes: []string{"h1:not-this-one", "zh:" + hex.EncodeToString(sum[:])}}
	in, srv := testInstaller(t, m)

	bin, err := in.install(context.Background(), thing, "1.2.0")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(in.cfg.PluginCacheDir, "registry.terraform.io/example/thing/1.2.0", runtime.GOOS+"_"+runtime.GOARCH, "terraform-provider-thing_v1.2.0"), bin)
	info, err := os.Stat(bin)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o100, "executable")
	assert.Equal(t, "Bearer secret", m.auth.Load())

	// The next start finds it in the cache, without the mirror.
	srv.Close()
	again, err := in.install(context.Background(), thing, "1.2.0")
	require.NoError(t, err)
	assert.Equal(t, bin, again)
	assert.Equal(t, int32(2), m.requests.Load())

	// Nothing left over beside it.
	entries, err := os.ReadDir(filepath.Dir(filepath.Dir(bin)))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestInstallAcceptsTheContentsHash(t *testing.T) {
	archive := zipOf(t, map[string]string{"terraform-provider-thing": "bin"})
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "terraform-provider-thing"), []byte("bin"), 0o600))
	h1, err := hashDir(dir)
	require.NoError(t, err)

	in, _ := testInstaller(t, &mirror{zip: archive, hashes: []string{h1}})
	_, err = in.install(context.Background(), thing, "1.2.0")
	require.NoError(t, err)
}

func TestInstallRefusesAnArchiveOfAnotherHash(t *testing.T) {
	in, _ := testInstaller(t, &mirror{
		zip:    zipOf(t, map[string]string{"terraform-provider-thing": "tampered"}),
		hashes: []string{"zh:" + strings.Repeat("0", 64)},
	})
	_, err := in.install(context.Background(), thing, "1.2.0")
	assert.ErrorContains(t, err, "checksum mismatch")
	assertEmptyCache(t, in)
}

func TestInstallRefusesAPathOutOfTheArchive(t *testing.T) {
	in, _ := testInstaller(t, &mirror{zip: zipOf(t, map[string]string{"../terraform-provider-thing": "x"})})
	_, err := in.install(context.Background(), thing, "1.2.0")
	assert.ErrorContains(t, err, "outside the archive")
	assertEmptyCache(t, in)
}

func TestInstallVersionTheMirrorDoesNotHave(t *testing.T) {
	// Artifactory answers {} for a version it does not have.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("{ }")); err != nil {
			panic(err)
		}
	}))
	t.Cleanup(srv.Close)
	in, _ := testInstaller(t, &mirror{})
	u, err := url.Parse(srv.URL + "/providers/")
	require.NoError(t, err)
	in.cfg.MirrorURL, in.client = u, srv.Client()
	_, err = in.install(context.Background(), thing, "1.2.2")
	assert.ErrorContains(t, err, "provider registry.terraform.io/example/thing 1.2.2: the mirror has no such version")
	assertEmptyCache(t, in)
}

func TestInstallOtherPlatformsOnly(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"archives": {"plan9_386": {"url": "x.zip"}, "aix_ppc64": {"url": "y.zip"}}}`)); err != nil {
			panic(err)
		}
	}))
	t.Cleanup(srv.Close)
	in, _ := testInstaller(t, &mirror{})
	u, err := url.Parse(srv.URL + "/providers/")
	require.NoError(t, err)
	in.cfg.MirrorURL, in.client = u, srv.Client()
	_, err = in.install(context.Background(), thing, "1.2.0")
	assert.ErrorContains(t, err, "the mirror has it for aix_ppc64, plan9_386 only, not "+runtime.GOOS+"_"+runtime.GOARCH)
}

func TestInstallUnknownVersion(t *testing.T) {
	in, _ := testInstaller(t, &mirror{})
	_, err := in.install(context.Background(), thing, "9.9.9")
	assert.ErrorContains(t, err, "404")
	assertEmptyCache(t, in)
}

// assertEmptyCache checks a failed install left nothing in the cache but
// empty directories.
func assertEmptyCache(t *testing.T, in *installer) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(in.cfg.PluginCacheDir, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		assert.True(t, d.IsDir(), "left behind: %s", path)
		return nil
	}))
}

func TestConfigSourceOrBinaryPath(t *testing.T) {
	const types = "resources:\n  thing_a: {}\n"
	f, err := parseConfig([]byte("provider:\n  source: example/thing\n  version: 1.2.0\n"+types), "x.yaml")
	require.NoError(t, err)
	assert.Equal(t, "thing", f.Provider.Name)

	for _, bad := range []string{
		"provider:\n  config: {}\n",
		"provider:\n  binary_path: /x/terraform-provider-thing\n  source: example/thing\n  version: 1.2.0\n",
		"provider:\n  source: example/thing\n",
		"provider:\n  source: example/thing\n  version: '>= 1.0'\n",
		"provider:\n  source: thing\n  version: 1.2.0\n",
	} {
		_, err := parseConfig([]byte(bad+types), "x.yaml")
		assert.Error(t, err, bad)
	}
}
