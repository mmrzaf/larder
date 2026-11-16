package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/integrity"
)

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo              string `json:"repo"`
		Sample            *int   `json:"sample"`
		RateMBs           *int   `json:"rate"`
		Repair            bool   `json:"repair"`
		DryRun            bool   `json:"dry_run"`
		CheckExpectations bool   `json:"check_expectations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	sample := 100
	if body.Sample != nil {
		sample = *body.Sample
	}
	if sample < 0 || sample > 100 {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "sample must be between 0 and 100")
		return
	}
	rate := 50
	if body.RateMBs != nil {
		rate = *body.RateMBs
	}
	if rate < 0 {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "rate must not be negative")
		return
	}
	options := integrity.ScrubOptions{
		Repo: body.Repo, Sample: sample, RateMBs: rate,
		DryRun: body.DryRun, CheckExpectations: body.CheckExpectations,
	}
	if body.Repair {
		options.Repair = s.repairReferences
	}
	s.startJob(w, "verify", func(ctx context.Context, job *Job) error {
		options.Progress = func(checked, bytes int64) {
			job.SetProgress(JobProgress{Items: checked, Bytes: bytes, Message: "verify"})
		}
		result, err := s.scrubber.Scan(ctx, options)
		if err != nil {
			return err
		}
		job.SetResult(result)
		return nil
	})
}

func (s *Server) repairReferences(ctx context.Context, refs []index.ObjectRef) (int, error) {
	rt := s.acquire()
	defer s.release(rt)
	if rt.engine.Offline() {
		return 0, nil
	}
	seen := make(map[string]bool)
	repaired := 0
	for _, ref := range refs {
		key := ref.Repo + "\x00" + ref.Key
		if seen[key] {
			continue
		}
		seen[key] = true
		requestPath, variant, _ := strings.Cut(ref.Key, "#")
		request := &http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Path: requestPath},
			Host:   "larder.internal",
			Header: make(http.Header),
		}
		switch variant {
		case "abbrev":
			request.Header.Set("Accept", "application/vnd.npm.install-v1+json")
		case "json":
			request.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
		case "html":
			request.Header.Set("Accept", "text/html")
		}
		var repoConfig *config.Repo
		for i := range rt.cfg.Repos {
			if rt.cfg.Repos[i].Name == ref.Repo {
				repoConfig = &rt.cfg.Repos[i]
				break
			}
		}
		if repoConfig == nil {
			continue
		}
		request = request.WithContext(ctx)
		recorder := httptest.NewRecorder()
		rt.engine.Handle(ctx, repoConfig, recorder, request)
		if recorder.Code >= 200 && recorder.Code < 300 {
			repaired++
		}
	}
	return repaired, nil
}
