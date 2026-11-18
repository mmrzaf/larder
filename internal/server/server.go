package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mmrzaf/larder/internal/cache"
	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/metrics"
	"github.com/mmrzaf/larder/internal/storage"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/internal/version"
)

// Options configures the HTTP server.
type Options struct {
	Config              *config.Config
	ConfigPath          string
	Store               *store.Store
	Index               *index.DB
	Kinds               *kinds.Registry
	Metrics             *metrics.Metrics
	Clock               cache.Clock
	Logger              *slog.Logger
	Bypass              cache.Bypass
	Storage             *storage.Manager
	ListenOverride      string
	AdminListenOverride string
	OfflineOverride     *bool
}

// Server owns the public and admin listeners, the current engine, and reload state.
type Server struct {
	baseOptions Options

	reloadMu   sync.Mutex
	current    atomic.Pointer[runtimeState]
	listenerMu sync.Mutex
	public     *http.Server
	admin      *http.Server
	publicLn   net.Listener
	adminLn    net.Listener
	errors     chan error

	jobs            *JobManager
	storage         *storage.Manager
	scrubber        *integrity.Scrubber
	maintainer      *integrity.Maintainer
	reconcileMu     sync.Mutex
	reconcileCancel context.CancelFunc
	reconcileWG     sync.WaitGroup
	maintenance     sync.Mutex
	captures        *captureStore
	requests        atomic.Int64
	hits            atomic.Int64
	shutdown        sync.Once
	shutdownErr     error
	started         bool
}

type runtimeState struct {
	cfg    *config.Config
	engine *cache.Engine
	logger *slog.Logger
	allow  []*net.IPNet

	mu      sync.Mutex
	refs    int
	retired bool
	done    chan struct{}
	closed  sync.Once
}

func (r *runtimeState) acquire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retired {
		return false
	}
	r.refs++
	return true
}

func (r *runtimeState) release() {
	r.mu.Lock()
	r.refs--
	closeNow := r.retired && r.refs == 0
	r.mu.Unlock()
	if closeNow {
		close(r.done)
	}
}

func (r *runtimeState) retire() {
	r.mu.Lock()
	if r.retired {
		r.mu.Unlock()
		return
	}
	r.retired = true
	closeNow := r.refs == 0
	r.mu.Unlock()
	if closeNow {
		close(r.done)
	}
}

func (r *runtimeState) closeEngine() {
	r.closed.Do(func() { _ = r.engine.Close() })
}

// New builds a server around the current configuration.
func New(opts Options) (*Server, error) {
	if opts.Config == nil || opts.Store == nil || opts.Index == nil || opts.Kinds == nil {
		return nil, fmt.Errorf("server: config, store, index and kinds are required")
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Metrics == nil {
		m, err := metrics.New(version.Get().Version)
		if err != nil {
			return nil, err
		}
		opts.Metrics = m
	}
	s := &Server{baseOptions: opts, errors: make(chan error, 2)}
	s.jobs = NewJobManager(opts.Clock, time.Hour)
	s.captures = newCaptureStore(opts.Clock)
	s.storage = opts.Storage
	if s.storage == nil {
		managerLogger := opts.Logger
		if managerLogger == nil {
			managerLogger = newLogger(opts.Config.Log)
		}
		manager, err := storage.New(storage.Options{
			Config: opts.Config, Store: opts.Store, Index: opts.Index,
			Metrics: opts.Metrics, Clock: opts.Clock, Logger: managerLogger,
		})
		if err != nil {
			return nil, err
		}
		s.storage = manager
	}
	maintenanceLogger := opts.Logger
	if maintenanceLogger == nil {
		maintenanceLogger = newLogger(opts.Config.Log)
	}
	scrubber, err := integrity.NewScrubber(opts.Store, opts.Index, opts.Metrics, maintenanceLogger, opts.Clock)
	if err != nil {
		return nil, err
	}
	s.scrubber = scrubber
	maintainer, err := integrity.NewMaintainer(integrity.MaintenanceOptions{
		Scrubber: scrubber, Index: opts.Index, DataDir: opts.Config.DataDir,
		Config: func() *config.Config {
			if current := s.current.Load(); current != nil {
				return current.cfg
			}
			return opts.Config
		},
		Clock: opts.Clock, Logger: maintenanceLogger,
		Prune: func(ctx context.Context) error {
			_, err := s.storage.Prune(ctx, storage.PruneOptions{Keep: -1})
			return err
		},
	})
	if err != nil {
		return nil, err
	}
	s.maintainer = maintainer
	rt, err := s.buildRuntime(s.effectiveConfig(opts.Config))
	if err != nil {
		return nil, err
	}
	s.current.Store(rt)
	return s, nil
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (s *Server) effectiveConfig(cfg *config.Config) *config.Config {
	copied := *cfg
	if s.baseOptions.ListenOverride != "" {
		copied.Listen = s.baseOptions.ListenOverride
	}
	if s.baseOptions.AdminListenOverride != "" {
		copied.Admin.Listen = s.baseOptions.AdminListenOverride
	}
	if s.baseOptions.OfflineOverride != nil {
		copied.Offline = *s.baseOptions.OfflineOverride
	}
	return &copied
}

func (s *Server) buildRuntime(cfg *config.Config) (*runtimeState, error) {
	logger := s.baseOptions.Logger
	if logger == nil {
		logger = newLogger(cfg.Log)
	}
	engine, err := cache.New(cache.Options{
		Config:  cfg,
		Store:   s.baseOptions.Store,
		Index:   s.baseOptions.Index,
		Kinds:   s.baseOptions.Kinds,
		Metrics: s.baseOptions.Metrics,
		Clock:   s.baseOptions.Clock,
		Logger:  logger,
		Bypass:  s.bypass(),
	})
	if err != nil {
		return nil, err
	}
	allow, err := parseCIDRs(cfg.Access.AllowCIDRs)
	if err != nil {
		_ = engine.Close()
		return nil, err
	}
	return &runtimeState{cfg: cfg, engine: engine, logger: logger, allow: allow, done: make(chan struct{})}, nil
}

func (s *Server) bypass() cache.Bypass {
	if s.baseOptions.Bypass != nil {
		return s.baseOptions.Bypass
	}
	return s.storage
}

func newLogger(cfg config.Log) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

func (s *Server) acquire() *runtimeState {
	for {
		rt := s.current.Load()
		if rt != nil && rt.acquire() {
			return rt
		}
	}
}

func (s *Server) release(rt *runtimeState) {
	rt.release()
}

// Start opens the listeners and begins serving.
func (s *Server) Start() error {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.started {
		return fmt.Errorf("server already started")
	}
	rt := s.current.Load()
	if rt == nil {
		return fmt.Errorf("server has no configuration")
	}
	publicLn, err := net.Listen("tcp", rt.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", rt.cfg.Listen, err)
	}
	var adminLn net.Listener
	if rt.cfg.Admin.Listen != "" {
		adminLn, err = net.Listen("tcp", rt.cfg.Admin.Listen)
		if err != nil {
			_ = publicLn.Close()
			return fmt.Errorf("listen admin %s: %w", rt.cfg.Admin.Listen, err)
		}
	}
	s.publicLn = publicLn
	s.adminLn = adminLn
	s.public = &http.Server{
		Handler:           s.PublicHandler(),
		ReadHeaderTimeout: rt.cfg.Timeouts.ReadHeaderTimeout.Duration(),
		IdleTimeout:       rt.cfg.Timeouts.IdleTimeout.Duration(),
		MaxHeaderBytes:    int(rt.cfg.MaxHeaderBytes),
	}
	go s.serve(s.public, publicLn)
	if adminLn != nil {
		s.admin = &http.Server{
			Handler:           s.AdminHandler(),
			ReadHeaderTimeout: rt.cfg.Timeouts.ReadHeaderTimeout.Duration(),
			IdleTimeout:       rt.cfg.Timeouts.IdleTimeout.Duration(),
			MaxHeaderBytes:    int(rt.cfg.MaxHeaderBytes),
		}
		go s.serve(s.admin, adminLn)
	}
	if adminLn != nil {
		if tcp, ok := adminLn.Addr().(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
			rt.logger.Warn("admin_listener_not_loopback", "address", tcp.String(),
				"message", "the admin API has no authentication; reach it only from a trusted network")
		}
	}
	s.started = true
	s.storage.Start()
	s.maintainer.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	s.reconcileMu.Lock()
	s.reconcileCancel = cancel
	s.reconcileMu.Unlock()
	s.reconcileWG.Add(1)
	go func() {
		defer s.reconcileWG.Done()
		s.reconcile(ctx)
	}()
	adminAddr := ""
	if adminLn != nil {
		adminAddr = adminLn.Addr().String()
	}
	rt.logger.Info("listening", "public", publicLn.Addr().String(), "admin", adminAddr)
	return nil
}

func (s *Server) reconcile(ctx context.Context) {
	result, err := integrity.Reconcile(ctx, s.baseOptions.Store, s.baseOptions.Index, s.baseOptions.Logger)
	if err != nil {
		if s.baseOptions.Logger != nil {
			s.baseOptions.Logger.Warn("reconcile_failed", "error", err)
		}
		return
	}
	if (result.ObjectsRemoved > 0 || result.OrphanFilesRemoved > 0) && s.baseOptions.Logger != nil {
		s.baseOptions.Logger.Info("reconcile_done", "objects_removed", result.ObjectsRemoved,
			"blob_rows_removed", result.BlobRowsRemoved, "orphan_files_removed", result.OrphanFilesRemoved)
	}
}

func (s *Server) serve(srv *http.Server, ln net.Listener) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		select {
		case s.errors <- err:
		default:
		}
	}
}

// Errors reports fatal listener errors.
func (s *Server) Errors() <-chan error { return s.errors }

// PublicAddr returns the bound public address.
func (s *Server) PublicAddr() string {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.publicLn == nil {
		return ""
	}
	return s.publicLn.Addr().String()
}

// AdminAddr returns the bound admin address, or empty when disabled.
func (s *Server) AdminAddr() string {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.adminLn == nil {
		return ""
	}
	return s.adminLn.Addr().String()
}

// CurrentConfig returns the active configuration.
func (s *Server) CurrentConfig() *config.Config {
	rt := s.current.Load()
	if rt == nil {
		return nil
	}
	return rt.cfg
}

// Jobs returns the admin job manager.
func (s *Server) Jobs() *JobManager { return s.jobs }

// Metrics returns the metric registry owner.
func (s *Server) Metrics() *metrics.Metrics { return s.baseOptions.Metrics }

// Reload re-reads the configuration file and swaps the engine atomically.
func (s *Server) Reload() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if s.baseOptions.ConfigPath == "" {
		return fmt.Errorf("reload: no configuration path")
	}
	cfg, err := config.Load(s.baseOptions.ConfigPath)
	if err != nil {
		return err
	}
	cfg = s.effectiveConfig(cfg)
	old := s.current.Load()
	if old != nil {
		if cfg.DataDir != old.cfg.DataDir {
			return fmt.Errorf("reload: data_dir cannot change without a restart")
		}
		if cfg.Listen != old.cfg.Listen {
			return fmt.Errorf("reload: listen address cannot change without a restart")
		}
		if cfg.Admin.Listen != old.cfg.Admin.Listen {
			return fmt.Errorf("reload: admin listen address cannot change without a restart")
		}
	}
	next, err := s.buildRuntime(cfg)
	if err != nil {
		return err
	}
	if old != nil {
		next.engine.SetOffline(old.engine.Offline())
	}
	s.storage.UpdateConfig(cfg)
	s.current.Store(next)
	if old != nil {
		old.retire()
		s.reap(old)
	}
	next.logger.Info("config_reloaded", "repos", len(cfg.Repos))
	return nil
}

func (s *Server) reap(rt *runtimeState) {
	go func() {
		<-rt.done
		rt.closeEngine()
	}()
}

// Shutdown gracefully stops both listeners and closes the current engine.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdown.Do(func() {
		s.reconcileMu.Lock()
		if s.reconcileCancel != nil {
			s.reconcileCancel()
		}
		s.reconcileMu.Unlock()
		s.reconcileWG.Wait()
		s.listenerMu.Lock()
		public := s.public
		admin := s.admin
		s.listenerMu.Unlock()
		var errs []error
		if public != nil {
			if err := public.Shutdown(ctx); err != nil {
				_ = public.Close()
				errs = append(errs, err)
			}
		}
		if admin != nil {
			if err := admin.Shutdown(ctx); err != nil {
				_ = admin.Close()
				errs = append(errs, err)
			}
		}
		rt := s.current.Swap(nil)
		if rt != nil {
			rt.retire()
			select {
			case <-rt.done:
			case <-ctx.Done():
				errs = append(errs, ctx.Err())
			}
			rt.closeEngine()
		}
		errs = append(errs, s.jobs.Close())
		errs = append(errs, s.maintainer.Close())
		errs = append(errs, s.storage.Close())
		s.shutdownErr = errors.Join(errs...)
	})
	return s.shutdownErr
}

// PublicHandler returns the public listener handler.
func (s *Server) PublicHandler() http.Handler {
	return http.HandlerFunc(s.handlePublic)
}

type trackingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *trackingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *trackingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) handlePublic(w http.ResponseWriter, r *http.Request) {
	rt := s.acquire()
	defer s.release(rt)
	tw := &trackingWriter{ResponseWriter: w}
	requestID := requestID(r, rt.cfg.TrustForwarded)
	tw.Header().Set("X-Request-Id", requestID)
	clientIP := clientIP(r, rt.cfg.TrustForwarded)
	start := time.Now()
	handedOff := false

	aborted := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				// The engine could not finish a response it had already begun.
				// Re-raised below so the connection is cut instead of ending
				// the body as if it were complete.
				aborted = true
				rt.logger.Warn("response_aborted", "request_id", requestID, "path", r.URL.Path, "bytes", tw.bytes)
			} else {
				rt.logger.Error("panic", "panic", recovered, "stack", string(debug.Stack()), "request_id", requestID, "path", r.URL.Path)
				if tw.status == 0 {
					writePlain(tw, http.StatusInternalServerError, "internal server error")
				}
			}
		}
		if aborted {
			defer panic(http.ErrAbortHandler)
		}
		if tw.status == 0 {
			tw.status = http.StatusOK
		}
		if handedOff {
			s.captures.collect(clientIP, r.Header.Get("X-Larder-Capture"), tw.Header().Get("X-Larder-Key"), tw.status)
			s.requests.Add(1)
			switch tw.Header().Get("X-Larder-Cache") {
			case "HIT", "REVALIDATED", "STALE", "OFFLINE-HIT":
				s.hits.Add(1)
			}
		}
		if !handedOff {
			rt.logger.Info("request",
				"repo", "",
				"method", r.Method,
				"path", r.URL.Path,
				"status", tw.status,
				"bytes", tw.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"cache", tw.Header().Get("X-Larder-Cache"),
				"class", "",
				"upstream", "",
				"stale", false,
				"verified", false,
				"client", clientIP,
				"request_id", requestID,
			)
		}
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		tw.Header().Set("Allow", "GET, HEAD")
		writePlain(tw, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !accessAllowed(rt.allow, clientIP) {
		writePlain(tw, http.StatusForbidden, "forbidden")
		return
	}
	normalized, err := NormalizePath(r.URL.EscapedPath())
	if err != nil {
		writePlain(tw, http.StatusBadRequest, "invalid path")
		return
	}
	repo, enginePath, err := route(rt.cfg, r, normalized)
	if err != nil {
		writePlain(tw, http.StatusNotFound, "not found")
		return
	}
	if err := checkFileHost(rt.cfg, repo, enginePath); err != nil {
		writePlain(tw, http.StatusForbidden, "forbidden")
		return
	}
	base := computePublicBase(rt.cfg, repo, r)
	ctx := cache.WithPublicBase(r.Context(), base)
	ctx = cache.WithRequestInfo(ctx, requestID, clientIP)
	request := r.Clone(ctx)
	request.URL.Path = enginePath
	request.URL.RawPath = ""
	handedOff = true
	rt.engine.Handle(ctx, repo, tw, request)
}

func writePlain(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}

func requestID(r *http.Request, trustForwarded bool) string {
	if trustForwarded {
		if value := strings.TrimSpace(r.Header.Get("X-Request-Id")); value != "" && len(value) <= 128 {
			return value
		}
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func clientIP(r *http.Request, trustForwarded bool) string {
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if trustForwarded {
		for _, part := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
			candidate := strings.TrimSpace(part)
			if ip := net.ParseIP(candidate); ip != nil {
				return ip.String()
			}
		}
	}
	if ip := net.ParseIP(remote); ip != nil {
		return ip.String()
	}
	return remote
}

func parseCIDRs(values []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("parse CIDR %q: %w", value, err)
		}
		out = append(out, network)
	}
	return out, nil
}

func accessAllowed(allow []*net.IPNet, address string) bool {
	if len(allow) == 0 {
		return true
	}
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	for _, network := range allow {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
