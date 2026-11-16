package cache

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/upstream"
	"github.com/mmrzaf/larder/testutil"
)

func TestRealClock(t *testing.T) {
	clock := realClock{}
	if clock.Now().IsZero() {
		t.Fatal("zero time")
	}
	select {
	case <-clock.After(0):
	case <-time.After(time.Second):
		t.Fatal("timer did not fire")
	}
}

func TestStatusWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	if _, err := sw.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	sw.WriteHeader(http.StatusTeapot)
	if sw.status != http.StatusOK || rec.Code != http.StatusOK || sw.bytes != 4 {
		t.Fatalf("status writer = %#v, recorder %d", sw, rec.Code)
	}
	sw.Flush()
	head := &statusWriter{ResponseWriter: httptest.NewRecorder(), head: true}
	if n, err := head.Write([]byte("ignored")); err != nil || n != 7 {
		t.Fatalf("head write = %d, %v", n, err)
	}
	if head.bytes != 0 {
		t.Fatalf("head bytes = %d", head.bytes)
	}
}

func TestNegativeCacheBounds(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	cache := newNegativeCache(clock, 2)
	cache.put("a", time.Minute, http.StatusNotFound, nil)
	cache.put("b", time.Minute, http.StatusNotFound, nil)
	if _, ok := cache.hit("a"); !ok {
		t.Fatal("entry a missing")
	}
	if _, ok := cache.hit("b"); !ok {
		t.Fatal("entry b missing")
	}
	cache.put("c", time.Minute, http.StatusNotFound, nil)
	if cache.len() != 2 {
		t.Fatalf("len = %d, want 2", cache.len())
	}
	if _, ok := cache.hit("a"); ok {
		t.Fatal("oldest entry was not evicted")
	}
	if _, ok := cache.hit("b"); !ok {
		t.Fatal("entry b missing")
	}
	if _, ok := cache.hit("c"); !ok {
		t.Fatal("entry c missing")
	}
	clock.Advance(2 * time.Minute)
	if _, ok := cache.hit("b"); ok {
		t.Fatal("expired entry hit")
	}
	if _, ok := cache.hit("c"); ok {
		t.Fatal("expired entry hit")
	}
	if cache.len() != 0 {
		t.Fatalf("expired len = %d", cache.len())
	}
	cache.put("d", time.Minute, http.StatusNotFound, nil)
	cache.hit("d")
	cache.put("d", time.Minute, http.StatusNotFound, nil)
}

func TestParseRangeTable(t *testing.T) {
	tests := []struct {
		value       string
		size        int64
		start       int64
		length      int64
		ok          bool
		unsatisfied bool
	}{
		{"bytes=0-0", 10, 0, 1, true, false},
		{"bytes=0-", 10, 0, 10, true, false},
		{"bytes=-3", 10, 7, 3, true, false},
		{"bytes=-20", 10, 0, 10, true, false},
		{"bytes=5-4", 10, 0, 0, false, true},
		{"bytes=99-", 10, 0, 0, false, true},
		{"bytes=0-1,3-4", 10, 0, 0, false, false},
		{"items=0-1", 10, 0, 0, false, false},
		{"bytes=-0", 10, 0, 0, false, true},
		{"bytes=x-1", 10, 0, 0, false, true},
	}
	for _, tt := range tests {
		start, length, ok, unsat := parseRange(tt.value, tt.size)
		if start != tt.start || length != tt.length || ok != tt.ok || unsat != tt.unsatisfied {
			t.Errorf("parseRange(%q) = %d,%d,%v,%v; want %d,%d,%v,%v", tt.value, start, length, ok, unsat, tt.start, tt.length, tt.ok, tt.unsatisfied)
		}
	}
}

func TestHelpers(t *testing.T) {
	if !isHTML("text/html; charset=utf-8") || isHTML("application/json") {
		t.Fatal("isHTML failed")
	}
	if !isText("application/json", "/x") || !isText("", "/x.json") || !isText("", "/Packages") {
		t.Fatal("isText failed")
	}
	if isText("application/octet-stream", "/x.bin") {
		t.Fatal("binary detected as text")
	}
	if contentTypeFor(kinds.Plan{Binary: true}, "") != "application/octet-stream" {
		t.Fatal("binary content type fallback")
	}
	if contentTypeFor(kinds.Plan{}, "text/plain") != "text/plain" {
		t.Fatal("stored content type not used")
	}
	for header, want := range map[string]bool{
		"gzip":       true,
		"br, gzip":   true,
		"gzip;q=0":   false,
		"gzip;q=0.5": true,
		"identity":   false,
		"":           false,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			req.Header.Set("Accept-Encoding", header)
		}
		if got := acceptsGzip(req); got != want {
			t.Errorf("acceptsGzip(%q) = %v", header, got)
		}
	}
	if !etagMatches(`"a", "b"`, `"b"`) || !etagMatches("*", `"b"`) || !etagMatches(`W/"b"`, `"b"`) || etagMatches(`"a"`, `"b"`) {
		t.Fatal("etagMatches failed")
	}
	if objectKey(kinds.Plan{Key: "/x", Variant: "full"}) != "/x#full" || objectKey(kinds.Plan{Key: "/x"}) != "/x" {
		t.Fatal("objectKey failed")
	}
	if companionPath("/x?y=1", ".sha256") != "/x.sha256?y=1" || companionPath("/x", ".sha256") != "/x.sha256" {
		t.Fatal("companionPath failed")
	}
}

func TestFlightWaitTimeoutAndDuplicate(t *testing.T) {
	group := newFlightGroup()
	flight, leader := group.begin("repo", "key")
	if !leader {
		t.Fatal("first begin was not leader")
	}
	if _, second := group.begin("repo", "key"); second {
		t.Fatal("second begin was leader")
	}
	if _, err := group.wait(context.Background(), flight, time.Nanosecond, realClock{}); err == nil {
		t.Fatal("wait did not time out")
	}
	group.finish("repo", "key", flight, flightResult{cache: "MISS"})
	if _, leader := group.begin("repo", "key"); !leader {
		t.Fatal("flight was not removed")
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestClientTee(t *testing.T) {
	tee := &clientTee{w: errorWriter{}, maxBackground: 3, contentLength: 10, method: http.MethodGet}
	if _, err := tee.Write([]byte("ab")); err != upstream.ErrStop {
		t.Fatalf("tee error = %v", err)
	}
}

func TestSinkBufferMethods(t *testing.T) {
	sink := &engineSink{
		repo: "repo", source: "source", logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	sink.Expect("key", "sha256", "not-hex")
	if len(sink.expectations) != 0 {
		t.Fatal("invalid expectation was buffered")
	}
	sink.MarkStale("/x*")
	if len(sink.stale) != 1 {
		t.Fatal("stale pattern was not buffered")
	}
	if sink.Logger() == nil {
		t.Fatal("nil logger")
	}
}

func TestMatrix_F16_FlightFinishIsOnceAndReleasesWaiters(t *testing.T) {
	g := newFlightGroup()
	flight, leader := g.begin("repo", "key")
	if !leader {
		t.Fatal("first caller is not the leader")
	}
	waiter, second := g.begin("repo", "key")
	if second || waiter != flight {
		t.Fatal("second caller did not join the flight")
	}
	first := flightResult{status: 502}
	g.finish("repo", "key", flight, first)
	// A deferred release after the leader already reported must change nothing.
	g.finish("repo", "key", flight, flightResult{status: 500})
	select {
	case <-waiter.done:
	default:
		t.Fatal("waiter was not released")
	}
	if waiter.result.status != 502 {
		t.Fatalf("result = %d, want the first report", waiter.result.status)
	}
	if _, leader := g.begin("repo", "key"); !leader {
		t.Fatal("key is still marked as in flight")
	}
}
