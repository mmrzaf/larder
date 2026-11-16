package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/transfer"
)

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To            string `json:"to"`
		Format        string `json:"format"`
		Repo          string `json:"repo"`
		Pinned        bool   `json:"pinned"`
		Match         string `json:"match"`
		OlderThanDays int    `json:"older_than_days"`
		BaseURL       string `json:"base_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if body.To == "" {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "to is required")
		return
	}
	s.startJob(w, "export", func(ctx context.Context, job *Job) error {
		cfg := s.CurrentConfig()
		if cfg == nil {
			return fmt.Errorf("export: no configuration")
		}
		snapshotDir := filepath.Join(cfg.DataDir, "snapshots")
		if err := os.MkdirAll(snapshotDir, 0o750); err != nil {
			return err
		}
		temp, err := os.CreateTemp(snapshotDir, ".export-*.db")
		if err != nil {
			return err
		}
		snapshotPath := temp.Name()
		_ = temp.Close()
		_ = os.Remove(snapshotPath)
		defer func() { _ = os.Remove(snapshotPath) }()
		if err := s.baseOptions.Index.VacuumInto(ctx, snapshotPath); err != nil {
			return err
		}
		idx, err := index.OpenReadOnly(snapshotPath)
		if err != nil {
			return err
		}
		defer func() { _ = idx.Close() }()
		result, err := transfer.Export(ctx, s.baseOptions.Store, idx, cfg, s.baseOptions.Kinds, transfer.ExportOptions{
			To: body.To, Format: body.Format, Repo: body.Repo, Match: body.Match,
			Pinned: body.Pinned, OlderThanDays: body.OlderThanDays, BaseURL: body.BaseURL,
			PinnedFunc: func(repo, key string) (bool, error) {
				return s.storage.IsPinned(ctx, repo, key)
			},
		})
		if err != nil {
			return err
		}
		job.SetResult(result)
		return nil
	})
}
