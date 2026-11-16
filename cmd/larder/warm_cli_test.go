package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/aptkind"
	"github.com/mmrzaf/larder/internal/kinds/cratekind"
	"github.com/mmrzaf/larder/internal/kinds/generickind"
	"github.com/mmrzaf/larder/internal/kinds/gokind"
	"github.com/mmrzaf/larder/internal/kinds/mavenkind"
	"github.com/mmrzaf/larder/internal/kinds/npmkind"
	"github.com/mmrzaf/larder/internal/kinds/pypikind"
	"github.com/mmrzaf/larder/internal/kinds/rustupkind"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/server"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/testutil"
)

type cliServer struct {
	cfg   *config.Config
	path  string
	srv   *server.Server
	fake  *testutil.FakeUpstream
	store *store.Store
	index *index.DB
	base  string
	admin string
}

func startCLIServer(t *testing.T, repos []config.Repo) *cliServer {
	t.Helper()
	fake := testutil.NewFakeUpstream(t)
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.Listen = "127.0.0.1:0"
	cfg.Admin.Listen = "127.0.0.1:0"
	cfg.Storage.MaxBytes = config.Size(1 << 40)
	cfg.Storage.MinFreeBytes = 0
	cfg.Storage.HardFreeBytes = 0
	cfg.Storage.Scrub.DailyPercent = 0
	for i := range repos {
		repo := &repos[i]
		if repo.Mount == "" {
			repo.Mount = "/" + repo.Name
		}
		if len(repo.Upstreams) == 0 {
			repo.Upstreams = []config.Upstream{{URL: fake.URL(), Proxy: config.ProxyDirect}}
		}
		if repo.MaxObjectSize == 0 {
			repo.MaxObjectSize = cfg.Limits.MaxObjectSize
		}
		if repo.NegativeTTL == 0 {
			repo.NegativeTTL = cfg.Cache.NegativeTTL
		}
		if repo.RevalidateTimeout == 0 {
			repo.RevalidateTimeout = cfg.Timeouts.RevalidateTotal
		}
		if repo.FreshTTL == 0 {
			repo.FreshTTL = config.Duration(60 * time.Second)
		}
		if repo.EvictWeight == 0 {
			repo.EvictWeight = 1
		}
		if repo.Proxy == "" {
			repo.Proxy = config.ProxyDirect
		}
	}
	cfg.Repos = repos
	path := filepath.Join(t.TempDir(), "larder.yaml")
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Open(filepath.Join(cfg.DataDir, "index.db"))
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	registry := kinds.NewRegistry()
	registrations := []struct {
		name string
		fn   func(config.Repo) (kinds.Kind, error)
	}{
		{"generic", generickind.New}, {"go", gokind.New}, {"npm", npmkind.New},
		{"pypi", pypikind.New}, {"crates", cratekind.New}, {"maven", mavenkind.New},
		{"rustup", rustupkind.New}, {"apt", aptkind.New},
	}
	for _, registration := range registrations {
		if err := registry.Register(registration.name, registration.fn); err != nil {
			t.Fatal(err)
		}
	}
	metricSet, err := metrics.New("test")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{
		Config: cfg, ConfigPath: path, Store: st, Index: idx,
		Kinds: registry, Metrics: metricSet,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ListenOverride: "127.0.0.1:0", AdminListenOverride: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	holder := &cliServer{
		cfg: cfg, path: path, srv: srv, fake: fake, store: st, index: idx,
		base: "http://" + srv.PublicAddr(), admin: "http://" + srv.AdminAddr(),
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
		_ = idx.Close()
		_ = st.Close()
	})
	return holder
}

func genericFilesRepo() config.Repo {
	return config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{
			DefaultClass: "deny", RejectHTML: true,
			Rules: []config.GenericRule{{Match: `^/files/.*`, Class: "immutable"}},
		},
	}
}

func TestMatrix_L08_WarmDryRunAndMaxBytes(t *testing.T) {
	ts := startCLIServer(t, []config.Repo{genericFilesRepo()})
	ts.fake.Set("/files/a", testutil.Route{Body: bytes.Repeat([]byte("a"), 100)})
	ts.fake.Set("/files/b", testutil.Route{Body: bytes.Repeat([]byte("b"), 200)})
	dir := t.TempDir()
	list := filepath.Join(dir, "urls.txt")
	if err := os.WriteFile(list, []byte("/files/a\n/files/b\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	baseArgs := []string{"-c", ts.path, "warm", "--server", ts.base, "--admin", ts.admin,
		"--url-list", list, "--repo", "generic"}
	code, stdout, stderr := runCLI(t, append(baseArgs, "--dry-run", "--json")...)
	if code != 0 {
		t.Fatalf("dry run exit %d: %s", code, stderr)
	}
	var plan struct {
		Plan struct {
			Items []struct {
				Path string `json:"path"`
			} `json:"items"`
		} `json:"plan"`
		TotalBytes int64 `json:"total_bytes"`
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("dry-run JSON: %v\n%s", err, stdout)
	}
	if len(plan.Plan.Items) != 2 || plan.TotalBytes != 300 {
		t.Fatalf("plan = %+v", plan)
	}
	code, _, _ = runCLI(t, append(baseArgs, "--dry-run", "--max-bytes", "150")...)
	if code != 2 {
		t.Fatalf("over-budget dry run exit %d, want 2", code)
	}
	if objects, err := ts.index.CurrentObjects(context.Background(), ""); err != nil || len(objects) != 0 {
		t.Fatalf("dry run stored objects: %d, %v", len(objects), err)
	}
	code, stdout, stderr = runCLI(t, append(baseArgs, "--max-bytes", "1000", "--json")...)
	if code != 0 {
		t.Fatalf("warm exit %d: %s", code, stderr)
	}
	var summary struct {
		Fetched int `json:"fetched"`
		Failed  int `json:"failed"`
	}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("warm JSON: %v\n%s", err, stdout)
	}
	if summary.Fetched != 2 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	code, stdout, _ = runCLI(t, append(baseArgs, "--json")...)
	if code != 0 {
		t.Fatalf("second warm exit %d", code)
	}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Fetched != 0 {
		t.Fatalf("cached warm fetched %d", summary.Fetched)
	}
}

func TestMatrix_L09_WarmPinSet(t *testing.T) {
	ts := startCLIServer(t, []config.Repo{genericFilesRepo()})
	ts.fake.Set("/files/pin", testutil.Route{Body: []byte("pinned content")})
	dir := t.TempDir()
	list := filepath.Join(dir, "urls.txt")
	if err := os.WriteFile(list, []byte("/files/pin\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "-c", ts.path, "warm", "--server", ts.base, "--admin", ts.admin,
		"--url-list", list, "--repo", "generic", "--pin-set", "warm-pins", "--json")
	if code != 0 {
		t.Fatalf("warm pin exit %d: %s", code, stderr)
	}
	var summary struct {
		Pinned int `json:"pinned"`
	}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Pinned != 1 {
		t.Fatalf("pinned = %d", summary.Pinned)
	}
	response, err := http.Get(ts.admin + "/_larder/v1/pins")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var pins struct {
		Pins []struct {
			Name           string `json:"name"`
			MatchedObjects int64  `json:"matched_objects"`
		} `json:"pins"`
	}
	if err := json.NewDecoder(response.Body).Decode(&pins); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pin := range pins.Pins {
		if pin.Name == "warm-pins" && pin.MatchedObjects == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("pin set missing: %+v", pins.Pins)
	}
}

func fakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o750); err != nil {
		t.Fatal(err)
	}
}

func TestMatrix_L10_SeedNativeToolsAndCapture(t *testing.T) {
	npmRepo := config.Repo{
		Name: "npm", Kind: "npm", Mount: "/npm",
		NPM: &config.NPMOptions{Audit: "passthrough", AuditTimeout: config.Duration(3 * time.Second)},
	}
	goRepo := config.Repo{Name: "go", Kind: "go", Mount: "/go", Go: &config.GoOptions{Verify: "basic"}}
	pypiRepo := config.Repo{Name: "pypi", Kind: "pypi", Mount: "/pypi", PyPI: &config.PyPIOptions{FilesHosts: []string{"files.pythonhosted.org"}}}
	cratesRepo := config.Repo{Name: "crates", Kind: "crates", Mount: "/crates", Crates: &config.CratesOptions{DLUpstreams: []string{"https://static.crates.io/crates/{crate}/{crate}-{version}.crate"}}}
	mavenRepo := config.Repo{Name: "maven", Kind: "maven", Mount: "/maven", Maven: &config.MavenOptions{CompanionFetch: true}}
	ts := startCLIServer(t, []config.Repo{npmRepo, goRepo, pypiRepo, cratesRepo, mavenRepo})

	tarball := []byte("left-pad tarball bytes")
	sha512sum := sha512.Sum512(tarball)
	sha1sum := sha1.Sum(tarball)
	packument := map[string]any{
		"name": "left-pad", "dist-tags": map[string]string{"latest": "1.3.0"},
		"versions": map[string]any{"1.3.0": map[string]any{
			"name": "left-pad", "version": "1.3.0",
			"dist": map[string]string{
				"tarball":   "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz",
				"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sha512sum[:]),
				"shasum":    hex.EncodeToString(sha1sum[:]),
			},
		}},
	}
	packData, _ := json.Marshal(packument)
	ts.fake.Set("/left-pad", testutil.Route{Body: packData, Header: http.Header{"Content-Type": []string{"application/json"}}})
	ts.fake.Set("/left-pad/-/left-pad-1.3.0.tgz", testutil.Route{Body: tarball})

	tools := t.TempDir()
	fakeTool(t, tools, "npm", "#!/bin/sh\nprintf '%s' '"+strings.ReplaceAll(`{"packages":{"node_modules/left-pad":{"version":"1.3.0","resolved":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}}}`, "'", "'\\''")+"' > package-lock.json\nexit 0\n")
	fakeTool(t, tools, "go", "#!/bin/sh\nexit 0\n")
	fakeTool(t, tools, "pip", "#!/bin/sh\nexit 0\n")
	fakeTool(t, tools, "cargo", "#!/bin/sh\nexit 0\n")
	fakeTool(t, tools, "gradle", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))

	seedFile := filepath.Join(t.TempDir(), "seed.yaml")
	body := `version: 1
pin_set: seed-pins
go:
  modules: [example.com/mod@v1.2.3]
npm:
  packages: ["left-pad@1.3.0"]
pypi:
  packages: ["requests>=2.32"]
crates:
  packages: ["serde@1"]
maven:
  coordinates: ["com.example:demo:1.0"]
`
	if err := os.WriteFile(seedFile, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "-c", ts.path, "seed", "--server", ts.base, "--admin", ts.admin, seedFile, "--json")
	if code != 0 {
		t.Fatalf("seed exit %d: %s\n%s", code, stderr, stdout)
	}
	var result struct {
		Sections []struct {
			Name    string `json:"name"`
			Failed  bool   `json:"failed"`
			Message string `json:"message"`
		} `json:"sections"`
		Summary struct {
			Pinned int `json:"pinned"`
			Failed int `json:"failed"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("seed JSON: %v\n%s", err, stdout)
	}
	if result.Summary.Pinned != 2 {
		t.Fatalf("pinned = %d, want 2 (%+v)", result.Summary.Pinned, result)
	}
	if result.Summary.Failed != 0 {
		t.Fatalf("summary failures = %+v", result)
	}
	if ts.fake.Count("/left-pad") == 0 || ts.fake.Count("/left-pad/-/left-pad-1.3.0.tgz") == 0 {
		t.Fatalf("npm artifacts were not warmed: %+v", ts.fake)
	}
}

func TestMatrix_L11_SeedMissingTool(t *testing.T) {
	npmRepo := config.Repo{
		Name: "npm", Kind: "npm", Mount: "/npm",
		NPM: &config.NPMOptions{Audit: "passthrough", AuditTimeout: config.Duration(3 * time.Second)},
	}
	pypiRepo := config.Repo{Name: "pypi", Kind: "pypi", Mount: "/pypi", PyPI: &config.PyPIOptions{FilesHosts: []string{"files.pythonhosted.org"}}}
	ts := startCLIServer(t, []config.Repo{npmRepo, pypiRepo})
	tarball := []byte("left-pad tarball bytes")
	sha512sum := sha512.Sum512(tarball)
	sha1sum := sha1.Sum(tarball)
	packument := map[string]any{
		"name": "left-pad", "dist-tags": map[string]string{"latest": "1.3.0"},
		"versions": map[string]any{"1.3.0": map[string]any{
			"name": "left-pad", "version": "1.3.0",
			"dist": map[string]string{
				"tarball":   "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz",
				"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sha512sum[:]),
				"shasum":    hex.EncodeToString(sha1sum[:]),
			},
		}},
	}
	packData, _ := json.Marshal(packument)
	ts.fake.Set("/left-pad", testutil.Route{Body: packData, Header: http.Header{"Content-Type": []string{"application/json"}}})
	ts.fake.Set("/left-pad/-/left-pad-1.3.0.tgz", testutil.Route{Body: tarball})
	tools := t.TempDir()
	fakeTool(t, tools, "npm", "#!/bin/sh\nprintf '%s' '"+strings.ReplaceAll(`{"packages":{"node_modules/left-pad":{"version":"1.3.0","resolved":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}}}`, "'", "'\\''")+"' > package-lock.json\nexit 0\n")
	t.Setenv("PATH", tools)

	seedFile := filepath.Join(t.TempDir(), "seed.yaml")
	body := `version: 1
pin_set: missing-tool
npm:
  packages: ["left-pad@1.3.0"]
pypi:
  packages: ["requests==2.31.0"]
`
	if err := os.WriteFile(seedFile, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "-c", ts.path, "seed", "--server", ts.base, "--admin", ts.admin, seedFile)
	if code != 1 {
		t.Fatalf("seed exit %d, want 1\n%s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "pip is not installed") {
		t.Fatalf("missing pip message absent:\n%s", stdout)
	}
	if !strings.Contains(stdout, "section npm: ok") {
		t.Fatalf("npm section did not continue:\n%s", stdout)
	}
}

func TestExportCLIWhileServerRuns(t *testing.T) {
	ts := startCLIServer(t, []config.Repo{genericFilesRepo()})
	ts.fake.Set("/files/a", testutil.Route{Body: []byte("export live")})
	response, err := http.Get(ts.base + "/generic/files/a")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	destination := filepath.Join(t.TempDir(), "live-export")
	code, stdout, stderr := runCLI(t, "-c", ts.path, "export", "--to", destination, "--json")
	if code != 0 {
		t.Fatalf("export exit %d: %s", code, stderr)
	}
	var result struct {
		Objects int64 `json:"objects"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("export JSON: %v\n%s", err, stdout)
	}
	if result.Objects != 1 {
		t.Fatalf("exported objects = %d", result.Objects)
	}
	if _, err := os.Stat(filepath.Join(destination, "larder-export.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDirectReportingWhileServerRuns(t *testing.T) {
	ts := startCLIServer(t, []config.Repo{genericFilesRepo()})
	ts.fake.Set("/files/a", testutil.Route{Body: []byte("report live")})
	response, err := http.Get(ts.base + "/generic/files/a")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	code, stdout, stderr := runCLI(t, "-c", ts.path, "du", "--by", "repo", "--json")
	if code != 0 {
		t.Fatalf("du exit %d: %s", code, stderr)
	}
	if !bytes.Contains([]byte(stdout), []byte("generic")) {
		t.Fatalf("du output = %s", stdout)
	}
	code, _, stderr = runCLI(t, "-c", ts.path, "ls", "generic")
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, stderr)
	}
}

func TestImportCLIWhileServerRuns(t *testing.T) {
	source := newCLIFixture(t, 1)
	source.close()
	tree := filepath.Join(t.TempDir(), "tree")
	code, _, stderr := runCLI(t, "-c", source.path, "export", "--to", tree)
	if code != 0 {
		t.Fatalf("export exit %d: %s", code, stderr)
	}
	target := startCLIServer(t, nil)
	code, stdout, stderr := runCLI(t, "-c", target.path, "import", "--from", tree, "--verify", "--json")
	if code != 0 {
		t.Fatalf("import exit %d: %s\n%s", code, stderr, stdout)
	}
	var result struct {
		Objects  int64 `json:"objects"`
		Verified int64 `json:"verified"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("import JSON: %v\n%s", err, stdout)
	}
	if result.Objects != 2 || result.Verified != 2 {
		t.Fatalf("import result = %+v", result)
	}
	if _, err := target.index.CurrentObject(context.Background(), "repo", "/object-00"); err != nil {
		t.Fatal(err)
	}
}

func TestMatrix_L12_IngestCLI(t *testing.T) {
	ts := startCLIServer(t, []config.Repo{genericFilesRepo()})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files", "a"), []byte("ingested bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "meta"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta", "b"), []byte("denied"), 0o640); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "-c", ts.path, "ingest", "generic", dir, "--json")
	if code != 0 {
		t.Fatalf("ingest exit %d: %s\n%s", code, stderr, stdout)
	}
	var result struct {
		Stored int64 `json:"stored"`
		Denied int64 `json:"denied"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("ingest JSON: %v\n%s", err, stdout)
	}
	if result.Stored != 1 || result.Denied != 1 {
		t.Fatalf("ingest result = %+v", result)
	}
	if _, err := ts.index.CurrentObject(context.Background(), "generic", "/files/a"); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.base + "/generic/files/a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "ingested bytes" {
		t.Fatalf("served ingested body = %q", body)
	}
}
