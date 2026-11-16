package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/storage"
)

func (s *Server) handleCaptureStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Set  string   `json:"set"`
		From []string `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	token, err := s.captures.start(body.Set, body.From)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (s *Server) handleCaptureStop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
		Pin   bool   `json:"pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	set, keys, pinned, err := s.captures.stop(body.Token, body.Pin, func(ctx context.Context, setName string, ref index.ObjectRef) error {
		err := s.storage.AddPin(ctx, storage.AddPinRequest{Set: setName, Repo: ref.Repo, Key: ref.Key})
		if errors.Is(err, index.ErrExists) {
			return nil
		}
		return err
	})
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"set": set, "keys": keys, "pinned": pinned,
	})
}
