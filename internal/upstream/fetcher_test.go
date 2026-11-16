package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/testutil"
)

func testOptions() Options {
	return Options{
		Timeouts:              config.Defaults().Timeouts,
		MaxConnsPerHost:       8,
		BlockPrivateRedirects: true,
	}
}

func newTestUpstream(t *testing.T, rawURL string) *Upstream {
	t.Helper()
	opts := testOptions()
	u, err := New(config.Upstream{URL: rawURL, Proxy: config.ProxyDirect}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(u.CloseIdleConnections)
	return u
}

func testFetcher(t *testing.T, clock Clock) *Fetcher {
	t.Helper()
	limits := config.Defaults().Limits
	breaker := config.Defaults().Breaker
	return NewFetcher(FetcherOptions{Clock: clock, Breaker: breaker, Limits: limits, Logger: nil})
}

func readBody(t *testing.T) Handler {
	return func(resp *Response) error {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	}
}

func TestFetcherFailover(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "second")
	}))
	defer second.Close()

	targets := []Target{
		{Repo: "r", Name: "first", Key: "first", URL: mustURL(t, first.URL+"/x"), upstream: newTestUpstream(t, first.URL)},
		{Repo: "r", Name: "second", Key: "second", URL: mustURL(t, second.URL+"/x"), upstream: newTestUpstream(t, second.URL)},
	}
	var body bytes.Buffer
	fetcher := testFetcher(t, testutil.NewFakeClock(time.Now()))
	result, err := fetcher.Fetch(context.Background(), http.MethodGet, targets, func(resp *Response) error {
		_, err := io.Copy(&body, resp.Body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if body.String() != "second" || result.Upstream != "second" {
		t.Fatalf("body = %q, result = %#v", body.String(), result)
	}
}

func TestFetcherNotFoundAndHandlerRetry(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFound.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "good")
	}))
	defer good.Close()

	fetcher := testFetcher(t, testutil.NewFakeClock(time.Now()))
	target := Target{Repo: "r", Name: "nf", Key: "nf", URL: mustURL(t, notFound.URL+"/x"), upstream: newTestUpstream(t, notFound.URL)}
	if _, err := fetcher.Fetch(context.Background(), http.MethodGet, []Target{target}, readBody(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not found error = %v", err)
	}

	targets := []Target{
		{Repo: "r", Name: "nf", Key: "nf2", URL: mustURL(t, notFound.URL+"/x"), upstream: newTestUpstream(t, notFound.URL)},
		{Repo: "r", Name: "good", Key: "good", URL: mustURL(t, good.URL+"/x"), upstream: newTestUpstream(t, good.URL)},
	}
	var body bytes.Buffer
	result, err := fetcher.Fetch(context.Background(), http.MethodGet, targets, func(resp *Response) error {
		if resp.Target.Name == "nf" {
			return fmt.Errorf("discard first")
		}
		_, err := io.Copy(&body, resp.Body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if body.String() != "good" || result.Upstream != "good" {
		t.Fatalf("body = %q, result = %#v", body.String(), result)
	}
}

func TestFetcherNotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, "body")
	}))
	defer server.Close()
	target := Target{Repo: "r", Name: "srv", Key: "srv", URL: mustURL(t, server.URL+"/x"), Headers: http.Header{"If-None-Match": []string{`"abc"`}}, upstream: newTestUpstream(t, server.URL)}
	result, err := testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{target}, func(resp *Response) error {
		if !resp.NotModified {
			return fmt.Errorf("expected 304")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.NotModified {
		t.Fatal("result is not marked not modified")
	}
}

func TestFetcherBreakerSkipsAndRecovers(t *testing.T) {
	var calls atomic.Int64
	var failing atomic.Bool
	failing.Store(true)
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "recovered")
	}))
	defer server.Close()
	breaker := config.Breaker{FailureThreshold: 2, OpenFor: config.Duration(30 * time.Second), OpenMax: config.Duration(5 * time.Minute)}
	limits := config.Defaults().Limits
	fetcher := NewFetcher(FetcherOptions{Clock: clock, Breaker: breaker, Limits: limits})
	target := Target{Repo: "r", Name: "srv", Key: "srv", URL: mustURL(t, server.URL+"/x"), upstream: newTestUpstream(t, server.URL)}

	for i := 0; i < 2; i++ {
		if _, err := fetcher.Fetch(context.Background(), http.MethodGet, []Target{target}, readBody(t)); err == nil {
			t.Fatalf("failure %d succeeded", i)
		}
	}
	before := calls.Load()
	if _, err := fetcher.Fetch(context.Background(), http.MethodGet, []Target{target}, readBody(t)); err == nil {
		t.Fatal("request succeeded while circuit was open")
	}
	if calls.Load() != before {
		t.Fatal("open circuit contacted the upstream")
	}

	clock.Advance(30 * time.Second)
	failing.Store(false)
	var body bytes.Buffer
	if _, err := fetcher.Fetch(context.Background(), http.MethodGet, []Target{target}, func(resp *Response) error {
		_, err := io.Copy(&body, resp.Body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if body.String() != "recovered" {
		t.Fatalf("body = %q", body.String())
	}
}

func TestFetcherConcurrencyCap(t *testing.T) {
	var active atomic.Int64
	var maxActive atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	limits := config.Defaults().Limits
	limits.MaxConcurrentFetches = 1
	limits.MaxUpstreamConnsPerHost = 1
	fetcher := NewFetcher(FetcherOptions{Clock: testutil.NewFakeClock(time.Now()), Breaker: config.Defaults().Breaker, Limits: limits})
	target := Target{Repo: "r", Name: "srv", Key: "srv", URL: mustURL(t, server.URL+"/x"), upstream: newTestUpstream(t, server.URL)}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = fetcher.Fetch(context.Background(), http.MethodGet, []Target{target}, readBody(t))
		}()
	}
	<-started
	select {
	case <-started:
		t.Fatal("second request started before the first completed")
	default:
	}
	close(release)
	wg.Wait()
	if maxActive.Load() != 1 {
		t.Fatalf("max active = %d, want 1", maxActive.Load())
	}
}

func TestFetcherStall(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "12345")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	u := newTestUpstream(t, server.URL)
	u.Timeouts.Stall = config.Duration(50 * time.Millisecond)
	fetcher := testFetcher(t, testutil.NewFakeClock(time.Now()))
	target := Target{Repo: "r", Name: "srv", Key: "srv", URL: mustURL(t, server.URL+"/x"), upstream: u}
	_, err := fetcher.Fetch(context.Background(), http.MethodGet, []Target{target}, func(resp *Response) error {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	})
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("stall error = %v", err)
	}
}

func TestUpstreamGzipAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("gzip was not accepted")
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte("compressed"))
		_ = gz.Close()
	}))
	defer server.Close()
	opts := testOptions()
	u, err := New(config.Upstream{URL: server.URL, Headers: map[string]string{"Authorization": "Bearer token"}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer u.CloseIdleConnections()
	target, err := u.Target("r", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	_, err = testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{target}, func(resp *Response) error {
		_, err := io.Copy(&body, resp.Body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if body.String() != "compressed" {
		t.Fatalf("body = %q", body.String())
	}
}

func TestUpstreamRedirectPolicy(t *testing.T) {
	received := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "target")
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	targetURL.Host = "localhost:" + targetURL.Port()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetURL.String(), http.StatusFound)
	}))
	defer redirector.Close()

	opts := testOptions()
	opts.BlockPrivateRedirects = false
	u, err := New(config.Upstream{URL: redirector.URL, Headers: map[string]string{"Authorization": "Bearer secret"}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	requestTarget, err := u.Target("r", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{requestTarget}, readBody(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != "" {
		t.Fatalf("authorization header was not dropped: %q", got)
	}

	opts.BlockPrivateRedirects = true
	blocked, err := New(config.Upstream{URL: redirector.URL}, opts)
	if err != nil {
		t.Fatal(err)
	}
	blockedTarget, err := blocked.Target("r", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{blockedTarget}, readBody(t)); err == nil {
		t.Fatal("private redirect was followed")
	}
}

func TestUpstreamCARoot(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tls")
	}))
	defer server.Close()
	cert := server.Certificate()
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemData, 0o640); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	u, err := New(config.Upstream{URL: server.URL, CAFile: path, Proxy: config.ProxyDirect}, opts)
	if err != nil {
		t.Fatal(err)
	}
	target, err := u.Target("r", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	if _, err := testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{target}, func(resp *Response) error {
		_, err := io.Copy(&body, resp.Body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if body.String() != "tls" {
		t.Fatalf("body = %q", body.String())
	}
}

func TestUpstreamTargetJoinAndValidation(t *testing.T) {
	u := newTestUpstream(t, "https://example.com/base")
	target, err := u.Target("r", "/a%2fb?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if target.URL.String() != "https://example.com/base/a%2fb?x=1" {
		t.Fatalf("target URL = %q", target.URL.String())
	}
	if _, err := New(config.Upstream{URL: "ftp://example.com"}, testOptions()); err == nil {
		t.Fatal("non-http upstream accepted")
	}
	if _, err := New(config.Upstream{URL: "https://example.com?x=1"}, testOptions()); err == nil {
		t.Fatal("upstream with query accepted")
	}
	if _, err := New(config.Upstream{URL: "https://example.com", CAFile: "/missing"}, testOptions()); err == nil {
		t.Fatal("missing CA file accepted")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUpstreamConnectTo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "connect-to")
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	u, err := New(config.Upstream{URL: "http://upstream.invalid:" + parsed.Port(), ConnectTo: parsed.Host}, opts)
	if err != nil {
		t.Fatal(err)
	}
	target, err := u.Target("r", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	if _, err := testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{target}, func(resp *Response) error {
		_, err := io.Copy(&body, resp.Body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if body.String() != "connect-to" {
		t.Fatalf("body = %q", body.String())
	}
}

func TestUpstreamRedirectLimit(t *testing.T) {
	var calls atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, server.URL+"/loop", http.StatusFound)
	}))
	defer server.Close()
	u, err := New(config.Upstream{URL: server.URL}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	target, err := u.Target("r", "/loop", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{target}, readBody(t))
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect limit error = %v", err)
	}
	if calls.Load() != 5 {
		t.Fatalf("redirect calls = %d, want 5", calls.Load())
	}
}

func TestUpstreamAccessorsAndTargets(t *testing.T) {
	u := newTestUpstream(t, "https://example.com/base")
	if u.Name() != "example.com" || u.Key() == "" {
		t.Fatalf("name/key = %q %q", u.Name(), u.Key())
	}
	target, err := u.Target("repo", "/a", nil)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := target.WithPath("/b", http.Header{"X-Test": []string{"1"}})
	if err != nil {
		t.Fatal(err)
	}
	if derived.URL.Path != "/base/b" || derived.Headers.Get("X-Test") != "1" {
		t.Fatalf("derived target = %#v", derived)
	}
	resolved, err := u.ResolvedTarget("repo", "https://other.example/c", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Name != "other.example" || resolved.URL.Path != "/c" {
		t.Fatalf("resolved target = %#v", resolved)
	}
	u.CloseIdleConnections()
}

func TestFetchErrors(t *testing.T) {
	empty := &FetchError{}
	if empty.Error() == "" || empty.Unwrap() != nil {
		t.Fatalf("empty fetch error = %q %v", empty.Error(), empty.Unwrap())
	}
	inner := errors.New("inner")
	agg := &FetchError{Attempts: []AttemptError{{Upstream: "u", Err: inner}}}
	if !errors.Is(agg, inner) || !strings.Contains(agg.Error(), "u") {
		t.Fatalf("aggregate error = %v", agg)
	}
	notFound := &NotFoundError{Status: 410}
	if !errors.Is(notFound, ErrNotFound) || !IsNotFound(notFound) || !strings.Contains(notFound.Error(), "410") {
		t.Fatalf("not found error = %v", notFound)
	}
	if IsNotFound(inner) {
		t.Fatal("ordinary error reported as not found")
	}
	if (&NotFoundError{}).Error() != ErrNotFound.Error() {
		t.Fatal("empty not found error")
	}
}

func TestRetryAfter(t *testing.T) {
	if got := retryAfter("10"); got != 10*time.Second {
		t.Fatalf("seconds = %v", got)
	}
	future := time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
	if got := retryAfter(future); got <= 0 {
		t.Fatalf("date = %v", got)
	}
	if got := retryAfter("bad"); got != 0 {
		t.Fatalf("invalid = %v", got)
	}
	if got := (realClock{}).Now(); got.IsZero() {
		t.Fatal("real clock zero")
	}
}

func TestFetcherMetrics(t *testing.T) {
	m, err := metrics.New("test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "metric")
	}))
	defer server.Close()
	limits := config.Defaults().Limits
	fetcher := NewFetcher(FetcherOptions{Clock: testutil.NewFakeClock(time.Now()), Breaker: config.Defaults().Breaker, Limits: limits, Metrics: m})
	target := Target{Repo: "repo", Name: "srv", Key: "srv", URL: mustURL(t, server.URL+"/x"), upstream: newTestUpstream(t, server.URL)}
	if _, err := fetcher.Fetch(context.Background(), http.MethodGet, []Target{target}, readBody(t)); err != nil {
		t.Fatal(err)
	}
	if m.UpstreamRequests.With("repo", "srv", "ok").Value() != 1 {
		t.Fatal("ok upstream metric")
	}
	if m.BytesFetched.With("repo", "srv").Value() != float64(len("metric")) {
		t.Fatal("bytes fetched metric")
	}
	if m.InflightFetches.With().Value() != 0 {
		t.Fatal("inflight metric not reset")
	}
	if m.UpstreamUp.With("srv").Value() != 1 {
		t.Fatal("upstream up metric")
	}
}

func TestStallReaderClose(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data"))
	reader := newStallReader(body, time.Second)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewFetcherDefaults(t *testing.T) {
	fetcher := NewFetcher(FetcherOptions{})
	if fetcher.global.capacity != 32 || fetcher.perHost != 8 {
		t.Fatalf("defaults = %d/%d", fetcher.global.capacity, fetcher.perHost)
	}
}

func TestUpstreamTimeoutOverrides(t *testing.T) {
	connect := config.Duration(2 * time.Second)
	u, err := New(config.Upstream{
		URL:      "https://example.com",
		Timeouts: &config.TimeoutOverrides{Connect: &connect},
	}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if u.Timeouts.Connect != connect {
		t.Fatalf("connect timeout = %v, want %v", u.Timeouts.Connect, connect)
	}
}

func TestRestrictedRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "target")
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	targetURL.Host = "localhost:" + targetURL.Port()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetURL.String(), http.StatusFound)
	}))
	defer redirector.Close()

	opts := testOptions()
	opts.BlockPrivateRedirects = false
	u, err := New(config.Upstream{URL: redirector.URL}, opts)
	if err != nil {
		t.Fatal(err)
	}
	base, err := u.Target("r", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	restricted := base
	restricted.RestrictHosts = true
	_, err = testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{restricted}, readBody(t))
	if err == nil || !strings.Contains(err.Error(), "outside the allowed set") {
		t.Fatalf("restricted redirect error = %v", err)
	}
	allowed := base
	if _, err := testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{allowed}, readBody(t)); err != nil {
		t.Fatalf("unrestricted redirect failed: %v", err)
	}
}

func TestMatrix_F12_UntrustedCertificateIsRejected(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "must not be delivered")
	}))
	defer server.Close()
	u := newTestUpstream(t, server.URL)
	target, err := u.Target("r", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = testFetcher(t, testutil.NewFakeClock(time.Now())).Fetch(context.Background(), http.MethodGet, []Target{target}, func(*Response) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("a server with an untrusted certificate was accepted (err=%v, called=%v)", err, called)
	}
}

func TestMatrix_F13_RefusedConnectionFailsAndCountsAgainstTheUpstream(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	address := closed.URL
	closed.Close()
	u := newTestUpstream(t, address)
	target, err := u.Target("r", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	f := testFetcher(t, testutil.NewFakeClock(time.Now()))
	for i := 0; i < 3; i++ {
		if _, err := f.Fetch(context.Background(), http.MethodGet, []Target{target}, readBody(t)); err == nil {
			t.Fatal("a refused connection reported success")
		}
	}
	states := f.States()
	if len(states) != 1 || !states[0].Open {
		t.Fatalf("circuit state after repeated connection failures = %+v", states)
	}
}

func TestMatrix_F14_LocalFailureDoesNotChargeOrFailOver(t *testing.T) {
	first := testutil.NewFakeUpstream(t)
	second := testutil.NewFakeUpstream(t)
	first.Set("/x", testutil.Route{Body: []byte("payload")})
	second.Set("/x", testutil.Route{Body: []byte("payload")})
	var targets []Target
	for _, fake := range []*testutil.FakeUpstream{first, second} {
		target, err := newTestUpstream(t, fake.URL()).Target("r", "/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	f := testFetcher(t, testutil.NewFakeClock(time.Now()))
	for i := 0; i < 5; i++ {
		_, err := f.Fetch(context.Background(), http.MethodGet, targets, func(*Response) error {
			return &LocalError{Err: errors.New("no space left on device")}
		})
		if !errors.Is(err, ErrStop) {
			t.Fatalf("local failure was not final: %v", err)
		}
	}
	if second.Total() != 0 {
		t.Fatalf("a local failure was retried against another upstream (%d requests)", second.Total())
	}
	for _, state := range f.States() {
		if state.Open || state.Failures != 0 {
			t.Fatalf("upstream charged for a local failure: %+v", state)
		}
	}
}
