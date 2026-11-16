// Package env builds a complete engine around fake upstreams for hermetic tests.
package env

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/cache"
	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/alpinekind"
	"github.com/mmrzaf/larder/internal/kinds/aptkind"
	"github.com/mmrzaf/larder/internal/kinds/cratekind"
	"github.com/mmrzaf/larder/internal/kinds/generickind"
	"github.com/mmrzaf/larder/internal/kinds/gokind"
	"github.com/mmrzaf/larder/internal/kinds/mavenkind"
	"github.com/mmrzaf/larder/internal/kinds/npmkind"
	"github.com/mmrzaf/larder/internal/kinds/pypikind"
	"github.com/mmrzaf/larder/internal/kinds/rustupkind"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/testutil"
)

// Options configures a test environment.
type Options struct {
	Repo                  config.Repo
	UpstreamCount         int
	Proxy                 config.Proxy
	Headers               map[string]string
	UpstreamBasePath      string
	CrateDLTemplates      []string
	Offline               bool
	Bypass                cache.Bypass
	BackgroundCompleteMax config.Size
	StaleWhileRevalidate  config.Duration
	MetadataHistory       int
	PublicBaseURL         string
	Clock                 *testutil.FakeClock
	Logger                *slog.Logger
	Metrics               *metrics.Metrics
}

// Environment wires config, store, index, kinds and the cache engine.
type Environment struct {
	Config   *config.Config
	Repo     config.Repo
	Store    *store.Store
	Index    *index.DB
	Engine   *cache.Engine
	Upstream *testutil.FakeUpstream
	Fakes    []*testutil.FakeUpstream
	Metrics  *metrics.Metrics
	Clock    *testutil.FakeClock

	closeOnce sync.Once
}

// New starts an environment and registers cleanup with the test.
func New(t testing.TB, opts Options) *Environment {
	t.Helper()
	if opts.UpstreamCount <= 0 {
		opts.UpstreamCount = 1
	}
	if opts.Clock == nil {
		opts.Clock = testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Metrics == nil {
		var err error
		opts.Metrics, err = metrics.New("test")
		if err != nil {
			t.Fatal(err)
		}
	}

	e := &Environment{Clock: opts.Clock, Metrics: opts.Metrics}
	for i := 0; i < opts.UpstreamCount; i++ {
		f := testutil.NewFakeUpstream(t)
		e.Fakes = append(e.Fakes, f)
	}
	e.Upstream = e.Fakes[0]

	cfg := config.Defaults()
	cfg.DataDir = testutil.TempDataDir(t)

	repo := opts.Repo
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
	if repo.Kind == "maven" && repo.Maven == nil {
		repo.Maven = &config.MavenOptions{CompanionFetch: true}
	}
	if repo.Kind == "go" && repo.Go == nil {
		repo.Go = &config.GoOptions{Verify: "basic"}
	}
	if repo.Kind == "npm" && repo.NPM == nil {
		repo.NPM = &config.NPMOptions{Audit: "passthrough", AuditTimeout: config.Duration(3 * time.Second)}
	}
	if repo.Kind == "pypi" && repo.PyPI == nil {
		repo.PyPI = &config.PyPIOptions{FilesHosts: []string{"files.pythonhosted.org"}}
	}
	if repo.Kind == "apt" && repo.APT == nil {
		repo.APT = &config.APTOptions{SuiteConsistency: true}
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
		if repo.Kind == "generic" {
			repo.FreshTTL = config.Duration(300 * time.Second)
		} else {
			repo.FreshTTL = config.Duration(60 * time.Second)
		}
	}
	if repo.Proxy == "" {
		repo.Proxy = cfg.UpstreamProxy
	}
	if repo.Kind == "crates" && len(opts.CrateDLTemplates) > 0 {
		if repo.Crates == nil {
			repo.Crates = &config.CratesOptions{}
		}
		repo.Crates.DLUpstreams = nil
		for _, template := range opts.CrateDLTemplates {
			repo.Crates.DLUpstreams = append(repo.Crates.DLUpstreams, e.Upstream.URL()+template)
		}
	}
	repo.Upstreams = nil
	for _, fake := range e.Fakes {
		repo.Upstreams = append(repo.Upstreams, config.Upstream{
			URL:     fake.URL() + opts.UpstreamBasePath,
			Proxy:   opts.Proxy,
			Headers: cloneMap(opts.Headers),
		})
	}

	cfg.Repos = []config.Repo{repo}
	cfg.Offline = opts.Offline
	if opts.BackgroundCompleteMax > 0 {
		cfg.Limits.BackgroundCompleteMax = opts.BackgroundCompleteMax
	}
	if opts.StaleWhileRevalidate > 0 {
		cfg.Cache.StaleWhileRevalidate = opts.StaleWhileRevalidate
	}
	if opts.MetadataHistory > 0 {
		cfg.Cache.MetadataHistory = opts.MetadataHistory
	}
	if opts.PublicBaseURL != "" {
		cfg.PublicBaseURL = opts.PublicBaseURL
	}
	e.Config = cfg
	e.Repo = repo

	var err error
	e.Store, err = store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	e.Index, err = index.Open(cfg.DataDir + "/index.db")
	if err != nil {
		_ = e.Store.Close()
		t.Fatal(err)
	}
	registry := kinds.NewRegistry()
	if err := registry.Register("generic", generickind.New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("test", NewTestKind); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("go", gokind.New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("maven", mavenkind.New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("npm", npmkind.New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("pypi", pypikind.New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("crates", cratekind.New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("rustup", rustupkind.New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("alpine", alpinekind.New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("apt", aptkind.New); err != nil {
		t.Fatal(err)
	}
	e.Engine, err = cache.New(cache.Options{
		Config:  cfg,
		Store:   e.Store,
		Index:   e.Index,
		Kinds:   registry,
		Metrics: opts.Metrics,
		Clock:   opts.Clock,
		Logger:  opts.Logger,
		Bypass:  opts.Bypass,
	})
	if err != nil {
		_ = e.Index.Close()
		_ = e.Store.Close()
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

// AbortedHeader is set on a recorded response whose connection the engine
// aborted after the response had started.
const AbortedHeader = "X-Test-Aborted"

// Serve runs one request with the engine and returns its recorder.
func (e *Environment) Serve(method, path string, headers http.Header) *httptest.ResponseRecorder {
	return e.ServeBody(method, path, headers, nil)
}

// ServeBody runs one request with a body.
func (e *Environment) ServeBody(method, path string, headers http.Header, body io.Reader) (rec *httptest.ResponseRecorder) {
	rec = httptest.NewRecorder()
	defer func() {
		// The server cuts the connection when a response it began cannot be
		// completed; the recorder stands in for that with a marker header.
		if recovered := recover(); recovered != nil {
			if recovered != http.ErrAbortHandler {
				panic(recovered)
			}
			rec.Header().Set(AbortedHeader, "1")
		}
	}()
	req := httptest.NewRequest(method, "http://larder.test"+path, body)
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	e.Engine.Handle(context.Background(), &e.Repo, rec, req)
	return rec
}

// GET is a convenience for a GET request.
func (e *Environment) GET(path string) *httptest.ResponseRecorder {
	return e.Serve(http.MethodGet, path, nil)
}

// Close releases the engine, index and store. It is safe to call twice.
func (e *Environment) Close() {
	e.closeOnce.Do(func() {
		if e.Engine != nil {
			_ = e.Engine.Close()
		}
		if e.Index != nil {
			_ = e.Index.Close()
		}
		if e.Store != nil {
			_ = e.Store.Close()
		}
	})
}

// Header builds a one-value header map.
func Header(name, value string) http.Header {
	h := make(http.Header)
	h.Set(name, value)
	return h
}

// Summary is a short description used in test failures.
func Summary(rec *httptest.ResponseRecorder) string {
	return fmt.Sprintf("status=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
