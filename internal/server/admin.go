package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/storage"
)

// AdminHandler returns the administrative listener handler.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /_larder/v1/stats", s.handleStats)
	mux.HandleFunc("GET /_larder/v1/du", s.handleDU)
	mux.HandleFunc("GET /_larder/v1/objects", s.handleObjects)
	mux.HandleFunc("GET /_larder/v1/objects/{repo}/{key...}", s.handleObject)
	mux.HandleFunc("GET /_larder/v1/pins", s.handlePins)
	mux.HandleFunc("POST /_larder/v1/pins", s.handleAddPin)
	mux.HandleFunc("DELETE /_larder/v1/pins/{set}", s.handleRemovePin)
	mux.HandleFunc("POST /_larder/v1/gc", s.handleGC)
	mux.HandleFunc("POST /_larder/v1/evict", s.handleEvict)
	mux.HandleFunc("POST /_larder/v1/prune", s.handlePrune)
	mux.HandleFunc("POST /_larder/v1/rollback", s.handleRollback)
	mux.HandleFunc("POST /_larder/v1/verify", s.handleVerify)
	mux.HandleFunc("POST /_larder/v1/export", s.handleExport)
	mux.HandleFunc("POST /_larder/v1/capture/start", s.handleCaptureStart)
	mux.HandleFunc("POST /_larder/v1/capture/stop", s.handleCaptureStop)
	mux.HandleFunc("GET /_larder/v1/upstreams", s.handleUpstreams)
	mux.HandleFunc("POST /_larder/v1/offline", s.handleOffline)
	mux.HandleFunc("POST /_larder/v1/reload", s.handleReload)
	mux.HandleFunc("GET /_larder/v1/jobs/{id}", s.handleJob)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				writeAdminError(w, http.StatusInternalServerError, "internal", fmt.Sprint(recovered))
			}
		}()
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	rt := s.acquire()
	defer s.release(rt)
	var reasons []string
	if err := s.baseOptions.Index.Ping(r.Context()); err != nil {
		reasons = append(reasons, "database: "+err.Error())
	}
	if err := unix.Access(rt.cfg.DataDir, unix.W_OK|unix.X_OK); err != nil {
		reasons = append(reasons, "data_dir: "+err.Error())
	}
	if free, err := s.baseOptions.Store.FreeSpace(); err != nil {
		reasons = append(reasons, "disk: "+err.Error())
	} else if free < uint64(rt.cfg.Storage.HardFreeBytes.Bytes()) {
		reasons = append(reasons, fmt.Sprintf("free disk %d below hard floor %d", free, rt.cfg.Storage.HardFreeBytes.Bytes()))
	}
	if len(reasons) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "reasons": reasons})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.storage != nil {
		_, _ = s.storage.Usage(r.Context(), false)
	}
	if s.baseOptions.Metrics == nil {
		return
	}
	if err := s.baseOptions.Metrics.WritePrometheus(w); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	rt := s.acquire()
	defer s.release(rt)
	stats, err := s.storage.Stats(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	requests := s.requests.Load()
	hits := s.hits.Load()
	ratio := 0.0
	if requests > 0 {
		ratio = float64(hits) / float64(requests)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"budget_bytes":        stats.BudgetBytes,
		"used_bytes":          stats.UsedBytes,
		"blob_bytes":          stats.BlobBytes,
		"index_bytes":         stats.IndexBytes,
		"snapshot_bytes":      stats.SnapshotBytes,
		"disk_free_bytes":     stats.FreeBytes,
		"disk_capacity_bytes": stats.CapacityBytes,
		"pinned_bytes":        stats.PinnedBytes,
		"objects":             stats.Objects,
		"blobs":               stats.Blobs,
		"pinned_over_budget":  stats.PinnedOverBudget,
		"bypass_active":       s.storage.BypassActive(),
		"offline":             rt.engine.Offline(),
		"requests_total":      requests,
		"hits_total":          hits,
		"hit_ratio":           ratio,
		"by_class":            stats.ByClass,
		"repos":               stats.ByRepo,
	})
}

func (s *Server) handleDU(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "repo"
	}
	n, err := intQuery(r, "n", 0)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	result, err := s.storage.DU(r.Context(), by, n)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleObjects(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, err := intQuery(r, "limit", 0)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	offset, err := intQuery(r, "offset", 0)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	objects, err := s.storage.List(r.Context(), storage.ListOptions{
		Repo: query.Get("repo"), Match: query.Get("match"), Sort: query.Get("sort"),
		Limit: limit, Offset: offset,
	})
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"objects": objects})
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	key := r.PathValue("key")
	if key != "" && key[0] != '/' {
		key = "/" + key
	}
	history := r.URL.Query().Get("history") == "true" || r.URL.Query().Get("history") == "1"
	details, err := s.storage.Details(r.Context(), repo, key, history)
	if errors.Is(err, index.ErrNotFound) {
		writeAdminError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, details)
}

func (s *Server) handlePins(w http.ResponseWriter, r *http.Request) {
	pins, err := s.storage.Pins(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": pins})
}

func (s *Server) handleAddPin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Set   string `json:"set"`
		Repo  string `json:"repo"`
		Key   string `json:"key"`
		Match string `json:"match"`
		Note  string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	err := s.storage.AddPin(r.Context(), storage.AddPinRequest{
		Set: body.Set, Repo: body.Repo, Key: body.Key, Match: body.Match, Note: body.Note,
	})
	if errors.Is(err, index.ErrExists) {
		writeAdminError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"pinned": true})
}

func (s *Server) handleRemovePin(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	err := s.storage.RemovePin(r.Context(), r.PathValue("set"), query.Get("repo"), query.Get("match"))
	if errors.Is(err, index.ErrNotFound) {
		writeAdminError(w, http.StatusNotFound, "not_found", "pin set or rule not found")
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}

func (s *Server) handleGC(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun      bool    `json:"dry_run"`
		TargetBytes *int64  `json:"target_bytes"`
		TargetPct   float64 `json:"target_pct"`
		Policy      string  `json:"policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	options := storage.GCOptions{
		DryRun: body.DryRun, Policy: body.Policy, Trigger: "manual",
		TargetPct: body.TargetPct,
	}
	if body.TargetBytes != nil {
		if *body.TargetBytes < 0 {
			writeAdminError(w, http.StatusBadRequest, "bad_request", "target_bytes must not be negative")
			return
		}
		options.HasTarget = true
		options.TargetBytes = *body.TargetBytes
	}
	if body.TargetPct < 0 || body.TargetPct > 100 {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "target_pct must be between 0 and 100")
		return
	}
	switch body.Policy {
	case "", "lru", "lfu", "score":
	default:
		writeAdminError(w, http.StatusBadRequest, "bad_request", "unknown eviction policy")
		return
	}
	s.startJob(w, "gc", func(ctx context.Context, job *Job) error {
		options.Progress = func(progress storage.Progress) {
			job.SetProgress(JobProgress{Items: progress.Items, Bytes: progress.Bytes, Message: progress.Message})
		}
		result, err := s.storage.GC(ctx, options)
		if err != nil {
			return err
		}
		job.SetResult(result)
		return nil
	})
}

func (s *Server) handleEvict(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo          string `json:"repo"`
		Glob          string `json:"glob"`
		IncludePinned bool   `json:"include_pinned"`
		OlderThanDays int    `json:"older_than_days"`
		LargerThan    int64  `json:"larger_than"`
		DryRun        bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if body.Repo == "" || body.Glob == "" {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "repo and glob are required")
		return
	}
	options := storage.EvictOptions{
		Repo: body.Repo, Glob: body.Glob, IncludePinned: body.IncludePinned,
		OlderThanDays: body.OlderThanDays, LargerThan: body.LargerThan, DryRun: body.DryRun,
	}
	s.startJob(w, "evict", func(ctx context.Context, job *Job) error {
		options.Progress = func(progress storage.Progress) {
			job.SetProgress(JobProgress{Items: progress.Items, Bytes: progress.Bytes, Message: progress.Message})
		}
		result, err := s.storage.Evict(ctx, options)
		if err != nil {
			return err
		}
		job.SetResult(result)
		return nil
	})
}

func (s *Server) startJob(w http.ResponseWriter, kind string, fn func(context.Context, *Job) error) {
	if !s.maintenance.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "conflict", "message": "job already running",
			"job": s.jobs.RunningID("maintenance"),
		})
		return
	}
	job, err := s.jobs.StartExclusive(kind, "maintenance", func(ctx context.Context, job *Job) error {
		defer s.maintenance.Unlock()
		return fn(ctx, job)
	})
	if errors.Is(err, ErrJobConflict) {
		s.maintenance.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "conflict", "message": "job already running",
			"job": s.jobs.RunningID("maintenance"),
		})
		return
	}
	if err != nil {
		s.maintenance.Unlock()
		writeAdminError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Location", "/_larder/v1/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"job": job.ID})
}

func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	var body struct {
		History bool   `json:"history"`
		Keep    *int   `json:"keep"`
		Repo    string `json:"repo"`
		DryRun  bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	keep := -1
	if body.Keep != nil {
		keep = *body.Keep
	}
	if !s.maintenance.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "conflict", "message": "job already running",
			"job": s.jobs.RunningID("maintenance"),
		})
		return
	}
	defer s.maintenance.Unlock()
	result, err := s.storage.Prune(r.Context(), storage.PruneOptions{Keep: keep, Repo: body.Repo, DryRun: body.DryRun})
	if errors.Is(err, storage.ErrBusy) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "conflict", "message": "job already running",
			"job": s.jobs.RunningID("maintenance"),
		})
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo       string `json:"repo"`
		Key        string `json:"key"`
		Generation int64  `json:"generation"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	object, err := s.storage.Rollback(r.Context(), body.Repo, body.Key, body.Generation)
	if errors.Is(err, index.ErrNotFound) {
		writeAdminError(w, http.StatusNotFound, "not_found", "generation not found")
		return
	}
	if errors.Is(err, storage.ErrBusy) {
		writeAdminError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repo": object.Repo, "key": object.Key, "generation": object.Generation,
	})
}

func intQuery(r *http.Request, name string, fallback int) (int, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return n, nil
}

func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	rt := s.acquire()
	defer s.release(rt)
	states := rt.engine.UpstreamStates()
	sort.Slice(states, func(i, j int) bool { return states[i].Upstream < states[j].Upstream })
	writeJSON(w, http.StatusOK, map[string]any{"upstreams": states})
}

func (s *Server) handleOffline(w http.ResponseWriter, r *http.Request) {
	rt := s.acquire()
	defer s.release(rt)
	var body struct {
		Offline *bool `json:"offline"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Offline == nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "body must be {\"offline\": true|false}")
		return
	}
	rt.engine.SetOffline(*body.Offline)
	writeJSON(w, http.StatusOK, map[string]bool{"offline": rt.engine.Offline()})
}

func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	if err := s.Reload(); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reloaded": true})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	view, err := s.jobs.Get(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
