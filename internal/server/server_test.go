package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/generickind"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/testutil"
)

type testServer struct {
	server     *Server
	cfg        *config.Config
	store      *store.Store
	index      *index.DB
	upstream   *testutil.FakeUpstream
	configPath string
}

type rewriteKind struct{}

func (rewriteKind) Name() string { return "test" }
func (rewriteKind) Classify(req kinds.Request) (kinds.Plan, error) {
	if req.Path != "/rewrite" {
		return kinds.Plan{Class: kinds.Deny, Key: req.Path, UpstreamPath: req.Path}, nil
	}
	return kinds.Plan{
		Class: kinds.Mutable, Key: "/rewrite", UpstreamPath: "/rewrite",
		Hooks: kinds.Hooks{Rewrite: func(raw []byte, publicBase string) ([]byte, error) {
			return []byte(publicBase + "|" + string(raw)), nil
		}},
	}, nil
}

type panicKind struct{}

func (panicKind) Name() string                               { return "panic" }
func (panicKind) Classify(kinds.Request) (kinds.Plan, error) { panic("test panic") }

type staticKind struct{ class kinds.Class }

func (k staticKind) Name() string { return "static" }
func (k staticKind) Classify(kinds.Request) (kinds.Plan, error) {
	return kinds.Plan{Class: k.class}, nil
}

func newTestServer(t *testing.T, repo config.Repo, mutate func(*config.Config)) *testServer {
	t.Helper()
	fake := testutil.NewFakeUpstream(t)
	if repo.Name == "" {
		repo.Name = "generic"
	}
	if repo.Kind == "" {
		repo.Kind = "generic"
	}
	if repo.Mount == "" {
		repo.Mount = "/" + repo.Name
	}
	if repo.Kind == "generic" && repo.Generic == nil {
		repo.Generic = &config.GenericOptions{DefaultClass: "deny"}
	}
	cfg := config.Defaults()
	cfg.DataDir = testutil.TempDataDir(t)
	cfg.Listen = "127.0.0.1:0"
	cfg.Admin.Listen = "127.0.0.1:0"
	repo.Upstreams = []config.Upstream{{URL: fake.URL(), Proxy: config.ProxyDirect}}
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
	if repo.Proxy == "" {
		repo.Proxy = config.ProxyDirect
	}
	if repo.EvictWeight == 0 {
		repo.EvictWeight = 1.0
	}
	cfg.Repos = []config.Repo{repo}
	if mutate != nil {
		mutate(cfg)
	}
	configPath := filepath.Join(t.TempDir(), "larder.yaml")
	writeServerConfig(t, configPath, cfg)

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
	if err := registry.Register("generic", generickind.New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("test", func(config.Repo) (kinds.Kind, error) { return rewriteKind{}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("pypi", func(config.Repo) (kinds.Kind, error) { return staticKind{class: kinds.Deny}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("panic", func(config.Repo) (kinds.Kind, error) { return panicKind{}, nil }); err != nil {
		t.Fatal(err)
	}
	m, err := metrics.New("test")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{
		Config: cfg, ConfigPath: configPath, Store: st, Index: idx,
		Kinds: registry, Metrics: m, Clock: testutil.NewFakeClock(time.Unix(1_700_000_000, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	ts := &testServer{server: srv, cfg: cfg, store: st, index: idx, upstream: fake, configPath: configPath}
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		_ = idx.Close()
		_ = st.Close()
	})
	return ts
}

func writeServerConfig(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
}

func (t *testServer) publicURL(path string) string {
	return "http://" + t.server.PublicAddr() + path
}

func (t *testServer) adminURL(path string) string {
	return "http://" + t.server.AdminAddr() + path
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"/a", "/a", false},
		{"//a///b", "/a/b", false},
		{"/a/../b", "", true},
		{"/a/%2e%2e/b", "", true},
		{"/a/%252e%252e/b", "", true},
		{"/a%2fb", "/a%2fb", false},
		{"/a\\b", "", true},
		{"/a%00b", "", true},
		{"/a/%1fb", "", true},
		{"/", "/", false},
		{"/a/", "/a/", false},
	}
	for _, tt := range tests {
		got, err := NormalizePath(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("NormalizePath(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("NormalizePath(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func FuzzNormalizePath(f *testing.F) {
	for _, seed := range []string{"/", "//a", "/a/../b", "/a/%2e%2e/b", "/a%2fb", "/a%00b", "/a\\b", "/%252e%252e"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := NormalizePath(input)
		if err != nil {
			return
		}
		if !strings.HasPrefix(got, "/") {
			t.Fatalf("result %q is not absolute", got)
		}
		for _, segment := range strings.Split(got, "/") {
			if segment == ".." || segment == "." {
				t.Fatalf("result %q contains a dot segment", got)
			}
		}
		if strings.ContainsAny(got, "\\\x00") {
			t.Fatalf("result %q contains a forbidden character", got)
		}
	})
}

func TestMatrix_C24_PathTraversal(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, nil)
	ts.upstream.Set("/file", testutil.Route{Body: []byte("x")})
	cases := []string{
		"/generic/../file",
		"/generic/%2e%2e/file",
		"/generic/%252e%252e/file",
		"/generic/a%5cb",
		"/generic/%00",
		"/generic/a%1fb",
	}
	for _, pathValue := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://larder.test"+pathValue, nil)
		ts.server.PublicHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", pathValue, rec.Code)
		}
	}
}

func TestMatrix_C25_FilesHostForbidden(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "pypi", Kind: "pypi", Mount: "/pypi",
		PyPI: &config.PyPIOptions{FilesHosts: []string{"files.pythonhosted.org"}},
	}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://larder.test/pypi/_files/evil.example/file", nil)
	ts.server.PublicHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden host status = %d", rec.Code)
	}
}

func TestMatrix_C26_Methods(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		rec := httptest.NewRecorder()
		ts.server.PublicHandler().ServeHTTP(rec, httptest.NewRequest(method, "http://larder.test/generic/file", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}
}

func TestMatrix_C36_AccessList(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, func(cfg *config.Config) {
		cfg.Access.AllowCIDRs = []string{"192.0.2.0/24"}
	})
	allowed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://larder.test/generic/file", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	ts.server.PublicHandler().ServeHTTP(allowed, req)
	if allowed.Code == http.StatusForbidden {
		t.Fatal("allowed client was denied")
	}
	denied := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://larder.test/generic/file", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	ts.server.PublicHandler().ServeHTTP(denied, req)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("denied client status = %d", denied.Code)
	}

	forwarded := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://larder.test/generic/file", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("X-Forwarded-For", "192.0.2.10")
	ts.server.PublicHandler().ServeHTTP(forwarded, req)
	if forwarded.Code != http.StatusForbidden {
		t.Fatalf("untrusted forwarded address status = %d", forwarded.Code)
	}
}

func TestMatrix_C37_HealthAndMetrics(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, nil)
	var response *http.Response
	var err error
	for _, pathValue := range []string{"/healthz", "/readyz"} {
		response, err = http.Get(ts.adminURL(pathValue))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d: %s", pathValue, response.StatusCode, body)
		}
	}
	if response, err := http.Get(ts.publicURL("/generic/file")); err != nil {
		t.Fatal(err)
	} else {
		_ = response.Body.Close()
	}
	response, err = http.Get(ts.adminURL("/metrics"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "larder_build_info") || !strings.Contains(string(body), "larder_requests_total") {
		t.Fatalf("metrics status = %d, body = %s", response.StatusCode, body)
	}
	public, err := http.Get(ts.publicURL("/healthz"))
	if err != nil {
		t.Fatal(err)
	}
	_ = public.Body.Close()
	if public.StatusCode == http.StatusOK {
		t.Fatal("admin health endpoint was served on the public listener")
	}
	stats, err := http.Get(ts.adminURL("/_larder/v1/stats"))
	if err != nil {
		t.Fatal(err)
	}
	var statsBody map[string]any
	if err := json.NewDecoder(stats.Body).Decode(&statsBody); err != nil {
		t.Fatal(err)
	}
	_ = stats.Body.Close()
	if statsBody["budget_bytes"] == nil {
		t.Fatal("stats missing budget")
	}
}

func TestMatrix_C44_HostRouting(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "test", Mount: "/test", Host: "npm.test",
		PublicURL: "https://public.test",
		Generic:   &config.GenericOptions{DefaultClass: "deny"},
	}, func(cfg *config.Config) {
		cfg.Routing = config.RoutingHost
	})
	ts.upstream.Set("/rewrite", testutil.Route{Body: []byte("raw")})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://larder.test/rewrite", nil)
	req.Host = "npm.test"
	ts.server.PublicHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "https://public.test|raw" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMatrix_C23_Reload(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, nil)
	before := ts.server.PublicAddr()
	next := *ts.cfg
	next.Repos = append([]config.Repo(nil), ts.cfg.Repos...)
	next.Repos[0].FreshTTL = config.Duration(5 * time.Second)
	next.Repos[0].Upstreams = append([]config.Upstream(nil), ts.cfg.Repos[0].Upstreams...)
	writeServerConfig(t, ts.configPath, &next)
	if err := ts.server.Reload(); err != nil {
		t.Fatal(err)
	}
	if ts.server.CurrentConfig().Repos[0].FreshTTL != config.Duration(5*time.Second) {
		t.Fatalf("fresh ttl = %v", ts.server.CurrentConfig().Repos[0].FreshTTL)
	}
	if ts.server.PublicAddr() != before {
		t.Fatal("reload changed the public listener")
	}
	if err := os.WriteFile(ts.configPath, []byte("version: ["), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ts.server.Reload(); err == nil {
		t.Fatal("invalid reload succeeded")
	}
	if ts.server.CurrentConfig().Repos[0].FreshTTL != config.Duration(5*time.Second) {
		t.Fatal("invalid reload replaced the old configuration")
	}
}

func TestIntegrationRealServer(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny", Rules: []config.GenericRule{{Match: `^/file$`, Class: "immutable"}}},
	}, nil)
	ts.upstream.Set("/file", testutil.Route{Body: []byte("real")})
	response, err := http.Get(ts.publicURL("/generic/file"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || string(body) != "real" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
	if response.Header.Get("X-Larder-Cache") != "MISS" {
		t.Fatalf("cache header = %q", response.Header.Get("X-Larder-Cache"))
	}
}

func TestServerShutdown(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ts.server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	addr := ts.server.PublicAddr()
	if addr == "" {
		t.Fatal("public address missing")
	}
	client := http.Client{Timeout: 100 * time.Millisecond}
	if _, err := client.Get("http://" + addr + "/generic/file"); err == nil {
		t.Fatal("server accepted a request after shutdown")
	}
}

func TestRouteBoundaries(t *testing.T) {
	cfg := config.Defaults()
	cfg.Repos = []config.Repo{
		{Name: "a", Mount: "/a"},
		{Name: "b", Mount: "/a/b"},
	}
	req := httptest.NewRequest(http.MethodGet, "http://x/a/b/c", nil)
	repo, pathValue, err := route(cfg, req, "/a/b/c")
	if err != nil || repo.Name != "b" || pathValue != "/c" {
		t.Fatalf("route = %#v %q %v", repo, pathValue, err)
	}
	req = httptest.NewRequest(http.MethodGet, "http://x/a/c", nil)
	repo, pathValue, err = route(cfg, req, "/a/c")
	if err != nil || repo.Name != "a" || pathValue != "/c" {
		t.Fatalf("route = %#v %q %v", repo, pathValue, err)
	}
}

func TestParseCIDRs(t *testing.T) {
	if _, err := parseCIDRs([]string{"bad"}); err == nil {
		t.Fatal("bad CIDR accepted")
	}
	allow, err := parseCIDRs([]string{"192.0.2.0/24"})
	if err != nil || !accessAllowed(allow, "192.0.2.4") || accessAllowed(allow, "198.51.100.4") {
		t.Fatal("CIDR parsing or matching failed")
	}
	if !accessAllowed(nil, "203.0.113.1") {
		t.Fatal("empty allow list denied a client")
	}
}

func TestMatrix_C38_GracefulShutdown(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, nil)
	rt := ts.server.current.Load()
	if rt == nil || !rt.acquire() {
		t.Fatal("could not acquire the runtime")
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- ts.server.Shutdown(ctx)
	}()
	select {
	case err := <-done:
		rt.release()
		t.Fatalf("shutdown returned before the active request finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	rt.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestRouteHostAndBoth(t *testing.T) {
	cfg := config.Defaults()
	cfg.Routing = config.RoutingBoth
	cfg.Repos = []config.Repo{
		{Name: "hosted", Mount: "/hosted", Host: "host.example"},
		{Name: "path", Mount: "/path"},
	}
	req := httptest.NewRequest(http.MethodGet, "http://host.example/foo", nil)
	repo, pathValue, err := route(cfg, req, "/foo")
	if err != nil || repo.Name != "hosted" || pathValue != "/foo" {
		t.Fatalf("host route = %#v %q %v", repo, pathValue, err)
	}
	req = httptest.NewRequest(http.MethodGet, "http://other/example/path/foo", nil)
	repo, pathValue, err = route(cfg, req, "/path/foo")
	if err != nil || repo.Name != "path" || pathValue != "/foo" {
		t.Fatalf("path fallback = %#v %q %v", repo, pathValue, err)
	}
	cfg.Routing = config.RoutingHost
	if _, _, err := route(cfg, req, "/path/foo"); err == nil {
		t.Fatal("host-only routing accepted an unknown host")
	}
}

func TestPublicBaseAndForwarded(t *testing.T) {
	cfg := config.Defaults()
	repo := config.Repo{PublicURL: "https://public.example"}
	req := httptest.NewRequest(http.MethodGet, "http://internal/", nil)
	if got := computePublicBase(cfg, &repo, req); got != "https://public.example" {
		t.Fatalf("repo base = %q", got)
	}
	cfg.PublicBaseURL = "https://global.example"
	repo.PublicURL = ""
	if got := computePublicBase(cfg, &repo, req); got != "https://global.example" {
		t.Fatalf("global base = %q", got)
	}
	cfg.PublicBaseURL = ""
	cfg.TrustForwarded = true
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "forwarded.example")
	if got := computePublicBase(cfg, &repo, req); got != "https://forwarded.example" {
		t.Fatalf("forwarded base = %q", got)
	}
}

func TestForwardedAccess(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, func(cfg *config.Config) {
		cfg.Access.AllowCIDRs = []string{"192.0.2.0/24"}
		cfg.TrustForwarded = true
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://larder.test/generic/file", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("X-Forwarded-For", "192.0.2.10")
	ts.server.PublicHandler().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("trusted forwarded address was denied")
	}
}

func TestPanicRecovery(t *testing.T) {
	ts := newTestServer(t, config.Repo{Name: "panic", Kind: "panic", Mount: "/panic"}, nil)
	rec := httptest.NewRecorder()
	ts.server.PublicHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://larder.test/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d", rec.Code)
	}
}

func TestAdminEndpoints(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, nil)
	response, err := http.Get(ts.adminURL("/_larder/v1/upstreams"))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upstreams status = %d", response.StatusCode)
	}
	response, err = http.Post(ts.adminURL("/_larder/v1/offline"), "application/json", strings.NewReader(`{"offline":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("offline status = %d", response.StatusCode)
	}
	if !ts.server.current.Load().engine.Offline() {
		t.Fatal("offline switch was not applied")
	}
	response, err = http.Get(ts.adminURL("/_larder/v1/jobs/missing"))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing job status = %d", response.StatusCode)
	}
}

func TestReadyzUnavailable(t *testing.T) {
	ts := newTestServer(t, config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{DefaultClass: "deny"},
	}, func(cfg *config.Config) {
		cfg.Storage.HardFreeBytes = config.Size(1 << 60)
	})
	response, err := http.Get(ts.adminURL("/readyz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d", response.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["reasons"] == nil {
		t.Fatal("readyz has no reasons")
	}
}
