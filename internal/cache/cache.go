// Package cache implements the cache engine: lookup, fetch, revalidate and serve.
package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/internal/upstream"
)

// Clock is the time source used by the engine.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Bypass tells the engine whether objects must not be stored while bypass is active.
type Bypass interface {
	Bypass(repo string) bool
}

// Reserver lets the storage layer refuse a download that would not fit,
// counting the space already promised to downloads in progress.
type Reserver interface {
	Reserve(size int64) (release func(), ok bool)
}

// Options configures an engine.
type Options struct {
	Config  *config.Config
	Store   *store.Store
	Index   *index.DB
	Kinds   *kinds.Registry
	Metrics *metrics.Metrics
	Clock   Clock
	Logger  *slog.Logger
	Access  *index.AccessRecorder
	Bypass  Bypass
}

// Engine serves repository requests using the index and blob store.
type Engine struct {
	cfg       *config.Config
	store     *store.Store
	index     *index.DB
	kinds     *kinds.Registry
	metrics   *metrics.Metrics
	clock     Clock
	logger    *slog.Logger
	bypass    Bypass
	access    *index.AccessRecorder
	ownAccess bool

	fetcher  *upstream.Fetcher
	repos    map[string]*repoRuntime
	flights  *flightGroup
	negative *negativeCache
	offline  atomic.Bool

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	retries sync.WaitGroup
}

type repoRuntime struct {
	cfg       config.Repo
	kind      kinds.Kind
	upstreams []*upstream.Upstream
	resolved  *upstream.Upstream
	allowed   []string
}

// New builds the engine.
func New(opts Options) (*Engine, error) {
	if opts.Config == nil || opts.Store == nil || opts.Index == nil || opts.Kinds == nil {
		return nil, fmt.Errorf("cache: config, store, index and kinds are required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		cfg:      opts.Config,
		store:    opts.Store,
		index:    opts.Index,
		kinds:    opts.Kinds,
		metrics:  opts.Metrics,
		clock:    clock,
		logger:   logger,
		bypass:   opts.Bypass,
		repos:    make(map[string]*repoRuntime),
		flights:  newFlightGroup(),
		negative: newNegativeCache(clock, 10000),
		ctx:      ctx,
		cancel:   cancel,
	}
	e.offline.Store(opts.Config.Offline)

	for i := range opts.Config.Repos {
		rt, err := e.buildRepo(&opts.Config.Repos[i])
		if err != nil {
			cancel()
			e.closeUpstreams()
			return nil, err
		}
		e.repos[rt.cfg.Name] = rt
	}
	e.fetcher = upstream.NewFetcher(upstream.FetcherOptions{
		Clock:   clock,
		Breaker: opts.Config.Breaker,
		Limits:  opts.Config.Limits,
		Metrics: opts.Metrics,
		Logger:  logger,
	})

	if opts.Access != nil {
		e.access = opts.Access
	} else {
		e.access = index.NewAccessRecorder(opts.Index, clock, 30*time.Second, logger)
		e.access.Start()
		e.ownAccess = true
	}
	e.setOfflineMetric()
	e.retries.Add(1)
	go e.retryLoop()
	return e, nil
}

func (e *Engine) buildRepo(repo *config.Repo) (*repoRuntime, error) {
	kind, err := e.kinds.New(*repo)
	if err != nil {
		return nil, fmt.Errorf("build kind for repo %s: %w", repo.Name, err)
	}
	allowed := make([]string, 0, len(repo.Upstreams)+4)
	for i := range repo.Upstreams {
		parsed, err := url.Parse(repo.Upstreams[i].URL)
		if err != nil {
			return nil, fmt.Errorf("repo %s upstream %d: %w", repo.Name, i, err)
		}
		allowed = append(allowed, parsed.Host)
	}
	if repo.PyPI != nil {
		allowed = append(allowed, repo.PyPI.FilesHosts...)
	}
	if repo.Kind == "crates" {
		templates := []string{"https://static.crates.io/crates/{crate}/{crate}-{version}.crate"}
		if repo.Crates != nil && len(repo.Crates.DLUpstreams) > 0 {
			templates = repo.Crates.DLUpstreams
		}
		for _, template := range templates {
			parsed, err := url.Parse(template)
			if err != nil {
				continue
			}
			if parsed.Host != "" {
				allowed = append(allowed, parsed.Host)
			}
		}
	}
	rt := &repoRuntime{cfg: *repo, kind: kind, allowed: allowed}
	for i := range repo.Upstreams {
		up, err := upstream.New(repo.Upstreams[i], upstream.Options{
			Timeouts:              e.cfg.Timeouts,
			MaxConnsPerHost:       e.cfg.Limits.MaxUpstreamConnsPerHost,
			BlockPrivateRedirects: e.cfg.Limits.BlockPrivateRedirects,
			AllowedHosts:          allowed,
		})
		if err != nil {
			return nil, fmt.Errorf("repo %s upstream %d: %w", repo.Name, i, err)
		}
		rt.upstreams = append(rt.upstreams, up)
	}
	resolved, err := upstream.New(config.Upstream{URL: "https://resolved.invalid", Proxy: repo.Proxy}, upstream.Options{
		Timeouts:              e.cfg.Timeouts,
		MaxConnsPerHost:       e.cfg.Limits.MaxUpstreamConnsPerHost,
		BlockPrivateRedirects: e.cfg.Limits.BlockPrivateRedirects,
		AllowedHosts:          allowed,
	})
	if err != nil {
		return nil, fmt.Errorf("repo %s resolved upstream: %w", repo.Name, err)
	}
	rt.resolved = resolved
	return rt, nil
}

// Close stops background work and releases engine-owned resources.
func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	e.cancel()
	e.wg.Wait()
	e.retries.Wait()
	e.closeUpstreams()
	if e.ownAccess && e.access != nil {
		return e.access.Close()
	}
	return nil
}

func (e *Engine) closeUpstreams() {
	for _, rt := range e.repos {
		for _, up := range rt.upstreams {
			up.CloseIdleConnections()
		}
		if rt.resolved != nil {
			rt.resolved.CloseIdleConnections()
		}
	}
}

// RetryUnverified rechecks unverified immutable objects whose kind can verify them.
func (e *Engine) RetryUnverified(ctx context.Context) error {
	objects, err := e.index.ListUnverified(ctx, 1000)
	if err != nil {
		return err
	}
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		rt := e.repos[object.Repo]
		if rt == nil || rt.kind.Name() != "go" {
			continue
		}
		pathValue, _, _ := strings.Cut(object.Key, "#")
		plan, err := rt.kind.Classify(kinds.Request{Path: pathValue})
		if err != nil || plan.Hooks.Verify == nil {
			continue
		}
		blob, err := e.index.GetBlob(ctx, object.SHA256)
		if err != nil {
			continue
		}
		file, err := e.store.OpenStored(object.SHA256)
		if err != nil {
			continue
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			continue
		}
		digest, verifyErr := plan.Hooks.Verify(ctx, file, info.Size(), e.verificationFetch(rt))
		closeErr := file.Close()
		if closeErr != nil {
			e.logger.Warn("verification_file_close_failed", "repo", object.Repo, "key", object.Key, "error", closeErr)
		}
		switch {
		case verifyErr == nil:
			if err := e.index.SetVerified(ctx, object.Repo, object.Key, true); err != nil {
				e.logger.Warn("verification_state_failed", "repo", object.Repo, "key", object.Key, "error", err)
				continue
			}
			if digest != "" {
				if err := e.index.UpsertExpectation(ctx, index.Expectation{
					Repo: object.Repo, Key: object.Key, Algo: "h1", Digest: digest,
					Source: "sumdb", CreatedAt: e.clock.Now(),
				}); err != nil {
					e.logger.Warn("expectation_store_failed", "repo", object.Repo, "key", object.Key, "error", err)
				}
			}
			e.logger.Info("verified", "repo", object.Repo, "key", object.Key, "blob", blob.SHA256)
		case errors.Is(verifyErr, kinds.ErrVerificationUnavailable):
		default:
			if err := e.removeUnverified(ctx, object); err != nil {
				e.logger.Warn("verification_remove_failed", "repo", object.Repo, "key", object.Key, "error", err)
			}
		}
	}
	return nil
}

func (e *Engine) retryLoop() {
	defer e.retries.Done()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-e.clock.After(time.Hour):
		}
		if err := e.RetryUnverified(e.ctx); err != nil && e.ctx.Err() == nil {
			e.logger.Warn("verification_retry_failed", "error", err)
		}
	}
}

// UpstreamStates returns the upstream circuit states.
func (e *Engine) UpstreamStates() []upstream.State { return e.fetcher.States() }

// Wait waits for background refreshes to finish.
func (e *Engine) Wait() { e.wg.Wait() }

// SetOffline changes the offline switch.
func (e *Engine) SetOffline(offline bool) {
	previous := e.offline.Swap(offline)
	if previous != offline {
		e.logger.Info("offline_changed", "offline", offline)
		e.setOfflineMetric()
	}
}

// Offline reports the current offline switch.
func (e *Engine) Offline() bool { return e.offline.Load() }

func (e *Engine) setOfflineMetric() {
	if e.metrics == nil {
		return
	}
	if e.offline.Load() {
		e.metrics.Offline.With().Set(1)
	} else {
		e.metrics.Offline.With().Set(0)
	}
}

type responseMeta struct {
	class      kinds.Class
	cache      string
	upstream   string
	stale      bool
	verified   bool
	goneStatus int
}

// Handle serves one request for a repository. The request path is after the mount.
func (e *Engine) Handle(ctx context.Context, repo *config.Repo, w http.ResponseWriter, r *http.Request) {
	rt := e.repos[repo.Name]
	if rt == nil {
		http.Error(w, "unknown repo", http.StatusNotFound)
		return
	}
	sw := &statusWriter{ResponseWriter: w, head: r.Method == http.MethodHead}
	w = sw
	meta := &responseMeta{}
	start := time.Now()
	defer e.finish(ctx, repo.Name, r, sw, meta, start)

	publicBase := e.publicBase(ctx, rt.cfg, r)
	plan, err := rt.kind.Classify(kinds.Request{
		Method:     r.Method,
		Path:       r.URL.Path,
		RawQuery:   r.URL.RawQuery,
		Header:     r.Header,
		PublicBase: publicBase,
	})
	if err != nil {
		status := http.StatusInternalServerError
		message := err.Error()
		if se, ok := err.(*kinds.StatusError); ok {
			status = se.Code
			message = se.Message()
		}
		writeError(w, status, "MISS", message)
		return
	}
	meta.class = plan.Class
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Method != http.MethodPost || !plan.AllowPost {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "MISS", "method not allowed")
			return
		}
	}
	key := objectKey(plan)
	if key != "" {
		w.Header().Set("X-Larder-Key", repo.Name+":"+key)
	}

	if plan.Redirect != "" {
		meta.cache = "MISS"
		w.Header().Set("X-Larder-Cache", "MISS")
		w.Header().Set("Cache-Control", "no-store")
		location := plan.Redirect
		if !strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "http") {
			location = "/" + location
		}
		http.Redirect(w, r, location, http.StatusMovedPermanently)
		return
	}
	switch plan.Class {
	case kinds.Deny:
		meta.cache = "MISS"
		writeError(w, http.StatusNotFound, "MISS", "denied")
	case kinds.Local:
		meta.cache = "BYPASS"
		e.handleLocal(w, plan)
	case kinds.Passthrough:
		e.handlePassthrough(ctx, rt, plan, w, r, meta)
	case kinds.Immutable, kinds.Mutable:
		e.handleObject(ctx, rt, plan, key, w, r, meta)
	default:
		writeError(w, http.StatusNotFound, "MISS", "unknown class")
	}
}

func (e *Engine) handleLocal(w http.ResponseWriter, plan kinds.Plan) {
	local := plan.Local
	if local == nil {
		writeError(w, http.StatusInternalServerError, "BYPASS", "missing local response")
		return
	}
	status := local.Status
	if status == 0 {
		status = http.StatusOK
	}
	for name, values := range local.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	contentType := local.ContentType
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Larder-Cache", "BYPASS")
	w.WriteHeader(status)
	_, _ = w.Write(local.Body)
}

func (e *Engine) handlePassthrough(ctx context.Context, rt *repoRuntime, plan kinds.Plan, w http.ResponseWriter, r *http.Request, meta *responseMeta) {
	meta.cache = "BYPASS"
	failureStatus := plan.FailureStatus
	if failureStatus == 0 {
		failureStatus = http.StatusBadGateway
	}
	if e.offline.Load() {
		offlineStatus := http.StatusGatewayTimeout
		if plan.FailureStatus != 0 {
			offlineStatus = plan.FailureStatus
		}
		writeError(w, offlineStatus, "OFFLINE-MISS", "offline: passthrough unavailable")
		return
	}
	targets, err := e.targets(rt, plan, nil)
	if err != nil {
		writeError(w, failureStatus, "BYPASS", err.Error())
		return
	}
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, e.cfg.Limits.MaxParseBytes.Bytes()+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "BYPASS", "read request body")
			return
		}
		if int64(len(body)) > e.cfg.Limits.MaxParseBytes.Bytes() {
			writeError(w, http.StatusRequestEntityTooLarge, "BYPASS", "request body too large")
			return
		}
		for i := range targets {
			targets[i].Body = body
			// The body is relayed as received, so a compressed body must keep
			// the header that says so; npm sends its audit request gzipped.
			if encoding := r.Header.Get("Content-Encoding"); encoding != "" {
				if targets[i].Headers == nil {
					targets[i].Headers = make(http.Header)
				}
				targets[i].Headers.Set("Content-Encoding", encoding)
			}
		}
	}
	requestCtx := e.ctx
	cancel := func() {}
	if plan.Timeout > 0 {
		requestCtx, cancel = context.WithTimeout(e.ctx, plan.Timeout)
	}
	defer cancel()
	started := false
	_, err = e.fetcher.Fetch(requestCtx, r.Method, targets, func(resp *upstream.Response) error {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		if !started {
			started = true
			meta.upstream = resp.Upstream
			w.Header().Set("X-Larder-Cache", "BYPASS")
			w.Header().Set("X-Larder-Upstream", resp.Upstream)
			w.Header().Set("Cache-Control", "no-store")
			if ct := resp.Header.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			if resp.ContentLength >= 0 {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
			}
			w.WriteHeader(http.StatusOK)
		}
		if r.Method == http.MethodHead {
			_, err := io.Copy(io.Discard, resp.Body)
			return err
		}
		if _, err := io.Copy(w, resp.Body); err != nil {
			return fmt.Errorf("%w: %w", upstream.ErrStop, err)
		}
		return nil
	})
	if err != nil {
		if started {
			abortResponse()
		}
		if !started {
			status := failureStatus
			if plan.FailureStatus == 0 && upstream.IsNotFound(err) {
				status = http.StatusNotFound
			}
			writeError(w, status, "BYPASS", "upstream request failed")
		}
		return
	}
}

func (e *Engine) handleObject(ctx context.Context, rt *repoRuntime, plan kinds.Plan, key string, w http.ResponseWriter, r *http.Request, meta *responseMeta) {
	object, err := e.index.CurrentObject(ctx, rt.cfg.Name, key)
	if err == nil {
		if e.storedIntact(ctx, rt, object) {
			e.handleStored(ctx, rt, plan, key, object, w, r, meta)
			return
		}
		err = index.ErrNotFound
	}
	if !errors.Is(err, index.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "MISS", "index lookup failed")
		return
	}
	if e.offline.Load() {
		meta.cache = "OFFLINE-MISS"
		writeError(w, e.cfg.OfflineMissStatus, "OFFLINE-MISS", "offline: "+rt.cfg.Name+":"+key)
		return
	}
	if entry, ok := e.negative.hit(rt.cfg.Name + "\x00" + key); ok {
		meta.cache = "MISS"
		if len(entry.body) > 0 {
			writeErrorBody(w, entry.status, "MISS", entry.body)
		} else {
			writeError(w, entry.status, "MISS", "not found")
		}
		return
	}
	if r.Method == http.MethodHead {
		e.handleHeadMiss(ctx, rt, plan, w, r, meta)
		return
	}
	if e.bypassActive(rt.cfg.Name) {
		e.handleBypass(ctx, rt, plan, w, r, meta)
		return
	}

	flight, leader := e.flights.begin(rt.cfg.Name, key)
	if !leader {
		e.waitAndServe(ctx, rt, plan, key, flight, w, r, meta)
		return
	}
	if current, err := e.index.CurrentObject(ctx, rt.cfg.Name, key); err == nil {
		result := flightResult{cache: "HIT", object: current, upstream: current.Upstream, verified: current.Verified}
		e.flights.finish(rt.cfg.Name, key, flight, result)
		e.applyFlight(rt, plan, key, result, w, r, meta, true)
		return
	}

	defer e.flights.finish(rt.cfg.Name, key, flight, flightResult{err: errLeaderFailed, status: http.StatusInternalServerError})
	var result flightResult
	switch plan.Class {
	case kinds.Immutable:
		result = e.fetchImmutable(ctx, rt, plan, key, w, r, meta)
	case kinds.Mutable:
		result = e.fetchMutable(ctx, rt, plan, key, w, r, meta)
	}
	e.flights.finish(rt.cfg.Name, key, flight, result)
	e.applyFlight(rt, plan, key, result, w, r, meta, true)
}

func (e *Engine) publicBase(ctx context.Context, repo config.Repo, r *http.Request) string {
	if base, ok := publicBaseFromContext(ctx); ok && base != "" {
		return strings.TrimSuffix(base, "/")
	}
	if repo.PublicURL != "" {
		return strings.TrimSuffix(repo.PublicURL, "/")
	}
	if e.cfg.PublicBaseURL != "" {
		return strings.TrimSuffix(e.cfg.PublicBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (e *Engine) finish(ctx context.Context, repo string, r *http.Request, sw *statusWriter, meta *responseMeta, start time.Time) {
	status := sw.status
	if status == 0 {
		status = http.StatusOK
	}
	result := resultName(meta.cache, status)
	class := "unknown"
	if meta.class != 0 {
		class = meta.class.String()
	}
	if e.metrics != nil {
		e.metrics.Requests.With(repo, class, result, fmt.Sprintf("%d", status)).Inc()
		e.metrics.RequestDuration.With(repo, class).Observe(time.Since(start).Seconds())
		e.metrics.BytesServed.With(repo).Add(float64(sw.bytes))
		if meta.stale {
			e.metrics.StaleServed.With(repo).Inc()
		}
	}
	if meta.cache == "HIT" && e.cfg.Log.SampleHits < 1 {
		if e.cfg.Log.SampleHits <= 0 || rand.Float64() >= e.cfg.Log.SampleHits {
			return
		}
	}
	info := requestInfoFromContext(ctx)
	e.logger.Info("request",
		"repo", repo,
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"bytes", sw.bytes,
		"duration_ms", time.Since(start).Milliseconds(),
		"cache", meta.cache,
		"class", class,
		"upstream", meta.upstream,
		"stale", meta.stale,
		"verified", meta.verified,
		"client", info.client,
		"request_id", info.id,
	)
}

func resultName(cache string, status int) string {
	switch cache {
	case "HIT":
		return "hit"
	case "MISS":
		return "miss"
	case "REVALIDATED":
		return "revalidated"
	case "STALE":
		return "stale"
	case "BYPASS":
		return "bypass"
	case "OFFLINE-HIT":
		return "offline_hit"
	case "OFFLINE-MISS":
		return "offline_miss"
	case "GONE":
		return "gone"
	}
	if status >= 500 {
		return "error"
	}
	return "miss"
}

type statusWriter struct {
	http.ResponseWriter
	head   bool
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.head {
		return len(p), nil
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeErrorBody(w http.ResponseWriter, status int, cache string, body []byte) {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if cache != "" {
		w.Header().Set("X-Larder-Cache", cache)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, cache string, message string) {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if cache != "" {
		w.Header().Set("X-Larder-Cache", cache)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}

func objectKey(plan kinds.Plan) string {
	if plan.Variant == "" {
		return plan.Key
	}
	return plan.Key + "#" + plan.Variant
}

func (e *Engine) bypassActive(repo string) bool {
	return e.bypass != nil && e.bypass.Bypass(repo)
}

func (e *Engine) targets(rt *repoRuntime, plan kinds.Plan, extra http.Header) ([]upstream.Target, error) {
	headers := make(http.Header)
	for name, values := range plan.UpstreamHdr {
		for _, value := range values {
			headers.Add(name, value)
		}
	}
	for name, values := range extra {
		for _, value := range values {
			headers.Set(name, value)
		}
	}
	if plan.Hooks.Resolve != nil {
		ups := make([]kinds.Upstream, 0, len(rt.upstreams))
		for _, up := range rt.upstreams {
			ups = append(ups, kinds.Upstream{URL: up.Base.String()})
		}
		candidates, err := plan.Hooks.Resolve(ups)
		if err != nil {
			return nil, err
		}
		targets := make([]upstream.Target, 0, len(candidates))
		for _, candidate := range candidates {
			candidateHeaders := headers.Clone()
			for name, values := range candidate.Header {
				candidateHeaders.Del(name)
				for _, value := range values {
					candidateHeaders.Add(name, value)
				}
			}
			client := rt.resolved
			parsed, err := url.Parse(candidate.URL)
			if err != nil {
				return nil, fmt.Errorf("parse resolved candidate: %w", err)
			}
			for _, up := range rt.upstreams {
				if strings.EqualFold(up.Base.Host, parsed.Host) {
					client = up
					break
				}
			}
			target, err := client.ResolvedTarget(rt.cfg.Name, candidate.URL, candidateHeaders)
			if err != nil {
				return nil, err
			}
			target.RestrictHosts = candidate.RestrictHosts
			targets = append(targets, target)
		}
		return targets, nil
	}
	targets := make([]upstream.Target, 0, len(rt.upstreams))
	for _, up := range rt.upstreams {
		target, err := up.Target(rt.cfg.Name, plan.UpstreamPath, headers)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, nil
}
