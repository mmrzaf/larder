package testutil

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Route is one programmable FakeUpstream route.
type Route struct {
	Status       int
	Body         []byte
	Header       http.Header
	ETag         string
	LastModified time.Time
	Conditional  bool
	Delay        time.Duration
	Stall        bool
	StallAfter   int
	Truncate     bool
	// BreakAfter sends that many body bytes without a Content-Length and then
	// drops the connection, as an upstream that dies mid-transfer would.
	BreakAfter      int
	Close           bool
	HTML            bool
	FailStatus      int
	Flap            int
	NotFoundFirst   int
	RequireHeader   map[string]string
	ForbiddenHeader string
	RequireBody     string
}

// FakeUpstream is an HTTP origin with programmable routes and fault injection.
type FakeUpstream struct {
	Server *httptest.Server

	mu     sync.Mutex
	routes map[string]Route
	counts map[string]int
}

// NewFakeUpstream starts a fake origin and closes it with the test.
func NewFakeUpstream(t testing.TB) *FakeUpstream {
	t.Helper()
	f := &FakeUpstream{routes: make(map[string]Route), counts: make(map[string]int)}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Server.Close)
	return f
}

// URL returns the base URL of the fake origin.
func (f *FakeUpstream) URL() string { return f.Server.URL }

// Set installs or replaces one route.
func (f *FakeUpstream) Set(path string, route Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = route
}

// Delete removes a route.
func (f *FakeUpstream) Delete(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.routes, path)
}

// Count returns the request count for a path.
func (f *FakeUpstream) Count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[path]
}

// Total returns all request counts.
func (f *FakeUpstream) Total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, count := range f.counts {
		total += count
	}
	return total
}

// ResetCounters clears request counters.
func (f *FakeUpstream) ResetCounters() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts = make(map[string]int)
}

func (f *FakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	route, ok := f.routes[r.URL.Path]
	f.counts[r.URL.Path]++
	count := f.counts[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	for name, value := range route.RequireHeader {
		if r.Header.Get(name) != value {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if route.ForbiddenHeader != "" && r.Header.Get(route.ForbiddenHeader) != "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if route.RequireBody != "" {
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || string(data) != route.RequireBody {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	if route.Delay > 0 {
		timer := time.NewTimer(route.Delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	if route.Flap > 0 && count <= route.Flap {
		status := route.FailStatus
		if status == 0 {
			status = http.StatusInternalServerError
		}
		http.Error(w, "flap", status)
		return
	}
	if route.NotFoundFirst > 0 && count <= route.NotFoundFirst {
		http.NotFound(w, r)
		return
	}
	if route.Conditional && route.ETag != "" {
		if r.Header.Get("If-None-Match") == route.ETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if !route.LastModified.IsZero() {
			if since, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil && !route.LastModified.After(since) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	if route.Close {
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
		return
	}

	for name, values := range route.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	if route.ETag != "" {
		w.Header().Set("ETag", route.ETag)
	}
	if !route.LastModified.IsZero() {
		w.Header().Set("Last-Modified", route.LastModified.UTC().Format(http.TimeFormat))
	}
	body := route.Body
	if route.HTML {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body = []byte("<html><body>error</body></html>")
	}
	status := route.Status
	if status == 0 {
		status = http.StatusOK
	}
	if route.BreakAfter > 0 && route.BreakAfter < len(body) {
		w.WriteHeader(status)
		_, _ = w.Write(body[:route.BreakAfter])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	if route.Truncate {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)+10))
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	if route.Stall {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		if route.StallAfter > 0 && route.StallAfter < len(body) {
			_, _ = w.Write(body[:route.StallAfter])
			body = body[route.StallAfter:]
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, bytes.NewReader(body))
	}
}
