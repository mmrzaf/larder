package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "larder.yaml")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireConfigErrors(t *testing.T, err error) Errors {
	t.Helper()
	if err == nil {
		t.Fatal("expected configuration error")
	}
	var errs Errors
	if !errors.As(err, &errs) {
		t.Fatalf("error is %T, want config.Errors: %v", err, err)
	}
	return errs
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(writeConfig(t, "version: 1\ndata_dir: "+dir+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 || c.Listen != "127.0.0.1:8080" || c.Admin.Listen != "127.0.0.1:9090" {
		t.Fatalf("unexpected defaults: %#v", c)
	}
	if c.Storage.HighWatermark != 0.90 || c.Storage.LowWatermark != 0.75 {
		t.Fatalf("unexpected watermarks: %#v", c.Storage)
	}
	if c.Cache.MetadataHistory != 2 || c.Limits.MaxObjectSize != Size(2<<30) {
		t.Fatalf("unexpected cache or limit defaults: %#v %#v", c.Cache, c.Limits)
	}
}

func TestLoadRepoDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := `
version: 1
data_dir: ` + dir + `
repos:
  - name: npm
    kind: npm
    upstreams:
      - url: https://registry.npmjs.org/
`
	c, err := Load(writeConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Repos) != 1 {
		t.Fatalf("got %d repos", len(c.Repos))
	}
	r := c.Repos[0]
	if r.Mount != "/npm" {
		t.Errorf("mount = %q, want /npm", r.Mount)
	}
	if r.FreshTTL != Duration(60_000_000_000) {
		t.Errorf("fresh_ttl = %v, want 60s", r.FreshTTL)
	}
	if r.RevalidateTimeout != c.Timeouts.RevalidateTotal {
		t.Errorf("revalidate_timeout = %v, want %v", r.RevalidateTimeout, c.Timeouts.RevalidateTotal)
	}
	if r.Proxy != ProxyDirect {
		t.Errorf("proxy = %q, want direct", r.Proxy)
	}
	if !r.ServeGone {
		t.Error("serve_gone default is false")
	}
	if r.NPM == nil || r.NPM.Audit != "passthrough" || r.NPM.AuditTimeout != Duration(3_000_000_000) {
		t.Errorf("npm defaults = %#v", r.NPM)
	}
}

func TestLoadExampleDocument(t *testing.T) {
	data, err := os.ReadFile("../../docs/05-configuration/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	content := strings.Replace(string(data), "data_dir: /data", "data_dir: "+dir, 1)
	c, err := Load(writeConfig(t, content))
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if len(c.Repos) != 12 {
		t.Fatalf("repos = %d, want 12", len(c.Repos))
	}
	if c.UpstreamProxy != Proxy("socks5h://127.0.0.1:1080") {
		t.Errorf("upstream_proxy = %q", c.UpstreamProxy)
	}
	if c.Storage.EvictionPolicy != "score" {
		t.Errorf("eviction policy = %q", c.Storage.EvictionPolicy)
	}
}

func TestLoadUnknownKeysReportPathAndSuggestion(t *testing.T) {
	dir := t.TempDir()
	cfg := `
version: 1
data_dir: ` + dir + `
log:
  levl: debug
repos:
  - name: generic
    kind: generic
    upstreams:
      - url: https://example.com
    fresh_tl: 5s
`
	_, err := Load(writeConfig(t, cfg))
	errs := requireConfigErrors(t, err)
	text := errs.Error()
	if !strings.Contains(text, "log.levl") || !strings.Contains(text, "did you mean \"level\"") {
		t.Errorf("missing log.levl suggestion:\n%s", text)
	}
	if !strings.Contains(text, "repos[0].fresh_tl") || !strings.Contains(text, "did you mean \"fresh_ttl\"") {
		t.Errorf("missing fresh_tl suggestion:\n%s", text)
	}
}

func TestLoadEnvExpansion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEST_DATA_DIR", dir)
	t.Setenv("TEST_TOKEN", "secret-token")
	t.Setenv("TEST_STATUS", "503")
	t.Setenv("TEST_BOOL", "true")
	cfg := `
version: 1
data_dir: ${TEST_DATA_DIR}
listen: ${MISSING_LISTEN:-127.0.0.1:9099}
offline_miss_status: ${TEST_STATUS}
trust_forwarded: ${TEST_BOOL}
repos:
  - name: generic
    kind: generic
    upstreams:
      - url: https://example.com
        headers:
          Authorization: Bearer ${TEST_TOKEN}
`
	c, err := Load(writeConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != dir {
		t.Errorf("data_dir = %q, want %q", c.DataDir, dir)
	}
	if c.Listen != "127.0.0.1:9099" {
		t.Errorf("listen = %q", c.Listen)
	}
	if c.OfflineMissStatus != 503 || !c.TrustForwarded {
		t.Errorf("numeric and boolean expansion failed: %#v", c)
	}
	if got := c.Repos[0].Upstreams[0].Headers["Authorization"]; got != "Bearer secret-token" {
		t.Errorf("header = %q", got)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LARDER_DATA_DIR", dir)
	t.Setenv("LARDER_LISTEN", "127.0.0.1:18080")
	t.Setenv("LARDER_ADMIN_LISTEN", "127.0.0.1:19090")
	t.Setenv("LARDER_LOG_LEVEL", "debug")
	c, err := Load(writeConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != dir || c.Listen != "127.0.0.1:18080" || c.Admin.Listen != "127.0.0.1:19090" || c.Log.Level != "debug" {
		t.Fatalf("env overrides not applied: %#v", c)
	}
}

func TestLoadCollectsParseErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := `
version: 1
data_dir: ` + dir + `
timeouts:
  connect: nope
limits:
  max_object_size: 1XB
storage:
  gc_interval: soon
`
	_, err := Load(writeConfig(t, cfg))
	errs := requireConfigErrors(t, err)
	text := errs.Error()
	for _, want := range []string{"timeouts.connect", "limits.max_object_size", "storage.gc_interval"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in:\n%s", want, text)
		}
	}
}

func TestLoadRejectsUnknownKindKey(t *testing.T) {
	dir := t.TempDir()
	cfg := `
version: 1
data_dir: ` + dir + `
repos:
  - name: npm
    kind: npm
    verify: strict
    upstreams:
      - url: https://registry.npmjs.org/
`
	_, err := Load(writeConfig(t, cfg))
	errs := requireConfigErrors(t, err)
	if !strings.Contains(errs.Error(), "repos[0].verify: only valid for kind go") {
		t.Fatalf("unexpected errors:\n%s", errs)
	}
}

func TestLoadRejectsNonMapping(t *testing.T) {
	_, err := Load(writeConfig(t, "- just\n- a list\n"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadRejectsEmpty(t *testing.T) {
	_, err := Load(writeConfig(t, ""))
	if err == nil {
		t.Fatal("expected error")
	}
}
