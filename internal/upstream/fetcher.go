package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/metrics"
)

var (
	// ErrNotFound means every upstream answered 404 or 410.
	ErrNotFound = errors.New("upstream object not found")
	// ErrStalled means a body made no progress within the stall timeout.
	ErrStalled = errors.New("upstream body stalled")
	// ErrNoTargets means a fetch had no URL to try.
	ErrNoTargets = errors.New("no upstream targets")
	// ErrStop asks the fetcher to stop without trying another upstream.
	ErrStop = errors.New("stop fetching")
)

// LocalError marks a failure of this process, such as a full disk or an
// index error, while it consumed an upstream response. The upstream is not at
// fault, so the fetcher neither charges its circuit breaker nor tries another
// upstream, which would fail the same way.
type LocalError struct{ Err error }

// Error implements error.
func (e *LocalError) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying error.
func (e *LocalError) Unwrap() error { return e.Err }

// NotFoundError reports that every upstream returned 404 or 410.
type NotFoundError struct {
	Status int
	Body   []byte
}

// Error implements error.
func (e *NotFoundError) Error() string {
	if e.Status == 0 {
		return ErrNotFound.Error()
	}
	return fmt.Sprintf("upstream returned %d", e.Status)
}

// Is lets errors.Is match ErrNotFound.
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// AttemptError records one failed upstream attempt.
type AttemptError struct {
	Upstream string
	Err      error
}

// FetchError is the aggregate of every failed attempt.
type FetchError struct {
	Attempts []AttemptError
}

// IsNotFound reports whether err means every upstream returned 404 or 410.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// Error implements error.
func (e *FetchError) Error() string {
	if len(e.Attempts) == 0 {
		return "all upstreams failed"
	}
	last := e.Attempts[len(e.Attempts)-1]
	return fmt.Sprintf("all upstreams failed: %s: %v", last.Upstream, last.Err)
}

// Unwrap exposes the last attempt error.
func (e *FetchError) Unwrap() error {
	if len(e.Attempts) == 0 {
		return nil
	}
	return e.Attempts[len(e.Attempts)-1].Err
}

// FetchResult describes a successful fetch.
type FetchResult struct {
	Target        Target
	StatusCode    int
	Header        http.Header
	ContentLength int64
	NotModified   bool
	Upstream      string
	URL           string
	Latency       time.Duration
}

// Response is passed to a fetch handler while the body is still open.
type Response struct {
	Target        Target
	StatusCode    int
	Header        http.Header
	ContentLength int64
	Body          io.Reader
	NotModified   bool
	Upstream      string
	URL           string
}

// Handler consumes a response. Returning an error discards the candidate and
// asks the fetcher to try the next upstream.
type Handler func(*Response) error

// FetcherOptions configures the shared upstream fetcher.
type FetcherOptions struct {
	Clock   Clock
	Breaker config.Breaker
	Limits  config.Limits
	Metrics *metrics.Metrics
	Logger  *slog.Logger
}

// Fetcher walks upstreams, applies breakers and concurrency caps, and invokes a handler per candidate.
type Fetcher struct {
	clock      Clock
	breakersMu sync.Mutex
	breakers   map[string]*breaker
	global     *limiter
	hostsMu    sync.Mutex
	hosts      map[string]*limiter
	healthMu   sync.Mutex
	health     map[string]healthState
	metrics    *metrics.Metrics
	logger     *slog.Logger
	breakerCfg config.Breaker
	perHost    int
}

// NewFetcher builds a fetcher.
func NewFetcher(opts FetcherOptions) *Fetcher {
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Limits.MaxConcurrentFetches <= 0 {
		opts.Limits.MaxConcurrentFetches = 32
	}
	if opts.Limits.MaxUpstreamConnsPerHost <= 0 {
		opts.Limits.MaxUpstreamConnsPerHost = 8
	}
	if opts.Breaker.FailureThreshold <= 0 {
		opts.Breaker.FailureThreshold = 3
	}
	if opts.Breaker.OpenFor <= 0 {
		opts.Breaker.OpenFor = config.Duration(30 * time.Second)
	}
	if opts.Breaker.OpenMax < opts.Breaker.OpenFor {
		opts.Breaker.OpenMax = config.Duration(5 * time.Minute)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Fetcher{
		clock:      opts.Clock,
		breakers:   make(map[string]*breaker),
		global:     newLimiter(opts.Limits.MaxConcurrentFetches),
		hosts:      make(map[string]*limiter),
		health:     make(map[string]healthState),
		metrics:    opts.Metrics,
		logger:     opts.Logger,
		breakerCfg: opts.Breaker,
		perHost:    opts.Limits.MaxUpstreamConnsPerHost,
	}
}

// Fetch tries each target in order until the handler succeeds.
func (f *Fetcher) Fetch(ctx context.Context, method string, targets []Target, handler Handler) (*FetchResult, error) {
	if len(targets) == 0 {
		return nil, ErrNoTargets
	}
	if method == "" {
		method = http.MethodGet
	}

	allOpen, allReady := f.circuitState(targets)
	forced := allOpen && allReady
	var attempts []AttemptError
	notFound := false
	notFoundStatus := 0
	var notFoundBody []byte

	for i := range targets {
		target := targets[i]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b := f.breaker(target.Key)
		if f.metrics != nil {
			if b.isOpen() {
				f.metrics.UpstreamUp.With(target.Name).Set(0)
			} else {
				f.metrics.UpstreamUp.With(target.Name).Set(1)
			}
		}
		if !forced {
			allowed, _ := b.allow()
			if !allowed {
				continue
			}
		}

		if err := f.global.acquire(ctx); err != nil {
			b.cancelProbe()
			return nil, err
		}
		hostSem := f.hostSem(target.URL.Hostname())
		if err := hostSem.acquire(ctx); err != nil {
			f.global.release()
			b.cancelProbe()
			return nil, err
		}
		start := time.Now()
		f.inflight(1)
		f.logger.Info("fetch_start", "repo", target.Repo, "upstream", target.Name)

		result, status, err := f.attempt(ctx, method, target, b, handler, start)
		f.inflight(-1)
		hostSem.release()
		f.global.release()

		if err == nil {
			return result, nil
		}
		if errors.Is(err, ErrStop) {
			b.cancelProbe()
			return nil, err
		}
		if errors.Is(err, ErrNotFound) {
			notFound = true
			var nf *NotFoundError
			if status == http.StatusGone || notFoundStatus != http.StatusGone {
				notFoundStatus = status
				if errors.As(err, &nf) {
					notFoundBody = nf.Body
				}
			}
			continue
		}
		if ctx.Err() != nil {
			b.cancelProbe()
			return nil, ctx.Err()
		}
		attempts = append(attempts, AttemptError{Upstream: target.Name, Err: err})
		if status == http.StatusTooManyRequests {
			continue
		}
	}

	if notFound && len(attempts) == 0 {
		return nil, &NotFoundError{Status: notFoundStatus, Body: notFoundBody}
	}
	if len(attempts) == 0 {
		return nil, &FetchError{}
	}
	return nil, &FetchError{Attempts: attempts}
}

func (f *Fetcher) attempt(ctx context.Context, method string, target Target, b *breaker, handler Handler, start time.Time) (*FetchResult, int, error) {
	attemptCtx := ctx
	if target.RestrictHosts {
		attemptCtx = context.WithValue(attemptCtx, restrictHostsKey{}, true)
	}
	cancel := func() {}
	if target.upstream.Timeouts.FetchTotal > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, target.upstream.Timeouts.FetchTotal.Duration())
	}
	defer cancel()

	var body io.Reader
	if len(target.Body) > 0 {
		body = bytes.NewReader(target.Body)
	}
	req, err := http.NewRequestWithContext(attemptCtx, method, target.URL.String(), body)
	if err != nil {
		return nil, 0, fmt.Errorf("create upstream request: %w", err)
	}
	for name, values := range target.upstream.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	for name, values := range target.Headers {
		req.Header.Del(name)
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("User-Agent", userAgent())

	resp, err := target.upstream.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		err = redactError(err, target.upstream.Proxy)
		f.recordFailure(target, outcomeForError(err))
		opened := b.failure()
		f.updateUp(target, b, opened)
		f.logger.Warn("fetch_failed", "repo", target.Repo, "upstream", target.Name, "error", err)
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	status := resp.StatusCode
	switch {
	case status == http.StatusOK:
		counted := &countingReader{Reader: resp.Body}
		stalled := newStallReader(counted, target.upstream.Timeouts.Stall.Duration())
		response := &Response{
			Target:        target,
			StatusCode:    status,
			Header:        resp.Header,
			ContentLength: resp.ContentLength,
			Body:          stalled,
			Upstream:      target.Name,
			URL:           target.URL.String(),
		}
		handlerErr := handler(response)
		_ = stalled.Close()
		f.bytesFetched(target, counted.count)
		if handlerErr != nil {
			if errors.Is(handlerErr, ErrStop) {
				return nil, status, handlerErr
			}
			var local *LocalError
			if errors.As(handlerErr, &local) {
				f.logger.Error("fetch_local_failure", "repo", target.Repo, "upstream", target.Name, "error", handlerErr)
				return nil, status, fmt.Errorf("%w: %w", ErrStop, handlerErr)
			}
			f.recordFailure(target, "invalid")
			opened := b.failure()
			f.updateUp(target, b, opened)
			f.logger.Warn("fetch_failed", "repo", target.Repo, "upstream", target.Name, "error", handlerErr)
			return nil, status, handlerErr
		}
		closed := b.success()
		f.updateUp(target, b, false)
		f.recordSuccess(target)
		if closed {
			f.logger.Info("upstream_circuit_closed", "upstream", target.Name)
		}
		f.logger.Info("fetch_done", "repo", target.Repo, "upstream", target.Name, "status", status, "bytes", counted.count, "duration_ms", latency.Milliseconds())
		return &FetchResult{
			Target: target, StatusCode: status, Header: resp.Header, ContentLength: resp.ContentLength,
			Upstream: target.Name, URL: target.URL.String(), Latency: latency,
		}, status, nil

	case status == http.StatusNotModified:
		response := &Response{
			Target: target, StatusCode: status, Header: resp.Header,
			ContentLength: resp.ContentLength, NotModified: true, Upstream: target.Name, URL: target.URL.String(),
		}
		handlerErr := handler(response)
		if handlerErr != nil {
			opened := b.failure()
			f.updateUp(target, b, opened)
			return nil, status, handlerErr
		}
		closed := b.success()
		f.updateUp(target, b, false)
		f.recordSuccess(target)
		if closed {
			f.logger.Info("upstream_circuit_closed", "upstream", target.Name)
		}
		f.logger.Info("fetch_done", "repo", target.Repo, "upstream", target.Name, "status", status, "duration_ms", latency.Milliseconds())
		return &FetchResult{
			Target: target, StatusCode: status, Header: resp.Header, NotModified: true,
			Upstream: target.Name, URL: target.URL.String(), Latency: latency,
		}, status, nil

	case status == http.StatusNotFound || status == http.StatusGone:
		b.success()
		f.updateUp(target, b, false)
		f.recordNotFound(target)
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		f.logger.Info("fetch_done", "repo", target.Repo, "upstream", target.Name, "status", status, "duration_ms", latency.Milliseconds())
		return nil, status, &NotFoundError{Status: status, Body: body}

	case status == http.StatusTooManyRequests:
		opened := b.failure()
		if retry := retryAfter(resp.Header.Get("Retry-After")); retry > 0 {
			b.openFor(retry)
		}
		f.updateUp(target, b, opened)
		f.recordFailure(target, "error")
		err := fmt.Errorf("upstream returned %d", status)
		f.logger.Warn("fetch_failed", "repo", target.Repo, "upstream", target.Name, "error", err)
		return nil, status, err

	default:
		opened := b.failure()
		f.updateUp(target, b, opened)
		f.recordFailure(target, "error")
		err := fmt.Errorf("upstream returned %d", status)
		f.logger.Warn("fetch_failed", "repo", target.Repo, "upstream", target.Name, "status", status)
		return nil, status, err
	}
}

func (f *Fetcher) circuitState(targets []Target) (allOpen, allReady bool) {
	seen := make(map[string]bool)
	allOpen = true
	allReady = true
	for _, target := range targets {
		if seen[target.Key] {
			continue
		}
		seen[target.Key] = true
		b := f.breaker(target.Key)
		if !b.isOpen() {
			allOpen = false
			allReady = false
			continue
		}
		if !b.ready() {
			allReady = false
		}
	}
	return allOpen && len(seen) > 0, allOpen && allReady && len(seen) > 0
}

func (f *Fetcher) breaker(key string) *breaker {
	f.breakersMu.Lock()
	defer f.breakersMu.Unlock()
	b := f.breakers[key]
	if b == nil {
		b = newBreaker(f.breakerCfg, f.clock)
		f.breakers[key] = b
	}
	return b
}

func (f *Fetcher) hostSem(host string) *limiter {
	f.hostsMu.Lock()
	defer f.hostsMu.Unlock()
	sem := f.hosts[host]
	if sem == nil {
		sem = newLimiter(f.perHost)
		f.hosts[host] = sem
	}
	return sem
}

func (f *Fetcher) updateUp(target Target, b *breaker, opened bool) {
	if f.metrics == nil {
		return
	}
	if b.isOpen() {
		f.metrics.UpstreamUp.With(target.Name).Set(0)
	} else {
		f.metrics.UpstreamUp.With(target.Name).Set(1)
	}
	if opened {
		f.logger.Warn("upstream_circuit_open", "upstream", target.Name)
	}
}

func (f *Fetcher) recordSuccess(target Target) {
	f.markHealth(target, true, "")
	if f.metrics == nil {
		return
	}
	f.metrics.UpstreamRequests.With(target.Repo, target.Name, "ok").Inc()
}

func (f *Fetcher) recordNotFound(target Target) {
	f.markHealth(target, true, "")
	if f.metrics == nil {
		return
	}
	f.metrics.UpstreamRequests.With(target.Repo, target.Name, "not_found").Inc()
}

func (f *Fetcher) recordFailure(target Target, outcome string) {
	f.markHealth(target, false, outcome)
	if f.metrics == nil {
		return
	}
	f.metrics.UpstreamRequests.With(target.Repo, target.Name, outcome).Inc()
}

func (f *Fetcher) markHealth(target Target, success bool, failure string) {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	state := f.health[target.Key]
	if success {
		state.LastSuccess = f.clock.Now()
		state.LastError = ""
	} else {
		state.LastError = failure
	}
	f.health[target.Key] = state
}

func (f *Fetcher) bytesFetched(target Target, n int64) {
	if f.metrics == nil || n == 0 {
		return
	}
	f.metrics.BytesFetched.With(target.Repo, target.Name).Add(float64(n))
}

func (f *Fetcher) inflight(delta int) {
	if f.metrics == nil {
		return
	}
	g := f.metrics.InflightFetches.With()
	if delta > 0 {
		g.Inc()
	} else {
		g.Dec()
	}
}

func outcomeForError(err error) string {
	if err == nil {
		return "error"
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return "timeout"
	}
	return "error"
}

func retryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

type countingReader struct {
	io.Reader
	count int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.count += int64(n)
	return n, err
}

func (r *countingReader) Close() error {
	if closer, ok := r.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

type stallReader struct {
	body     io.ReadCloser
	stall    time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	timedOut bool
	closed   bool
}

func newStallReader(body io.ReadCloser, stall time.Duration) *stallReader {
	return &stallReader{body: body, stall: stall}
}

func (r *stallReader) Read(p []byte) (int, error) {
	if r.stall > 0 {
		r.arm()
	}
	n, err := r.body.Read(p)
	r.disarm()
	if r.isTimedOut() {
		return n, fmt.Errorf("%w after %s", ErrStalled, r.stall)
	}
	return n, err
}

func (r *stallReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.mu.Unlock()
	return r.body.Close()
}

func (r *stallReader) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timedOut || r.closed {
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(r.stall, func() {
		r.mu.Lock()
		r.timedOut = true
		r.mu.Unlock()
		_ = r.body.Close()
	})
}

func (r *stallReader) disarm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}

func (r *stallReader) isTimedOut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timedOut
}

// State is the observable health of one upstream circuit.
type State struct {
	Upstream    string    `json:"upstream"`
	Open        bool      `json:"open"`
	RetryAt     time.Time `json:"retry_at,omitempty"`
	Failures    int       `json:"failures"`
	LastError   string    `json:"last_error,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
}

type healthState struct {
	LastError   string
	LastSuccess time.Time
}

// States returns the circuit state for every upstream seen by the fetcher.
func (f *Fetcher) States() []State {
	f.breakersMu.Lock()
	defer f.breakersMu.Unlock()
	out := make([]State, 0, len(f.breakers))
	f.healthMu.Lock()
	health := make(map[string]healthState, len(f.health))
	for key, value := range f.health {
		health[key] = value
	}
	f.healthMu.Unlock()
	for key, b := range f.breakers {
		open, until, _, failures := b.state()
		h := health[key]
		out = append(out, State{
			Upstream: key, Open: open, RetryAt: until, Failures: failures,
			LastError: h.LastError, LastSuccess: h.LastSuccess,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Upstream < out[j].Upstream })
	return out
}
