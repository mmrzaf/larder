package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientEndpoints(t *testing.T) {
	var offline bool
	mux := http.NewServeMux()
	mux.HandleFunc("/_larder/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"used_bytes": 7})
	})
	mux.HandleFunc("/_larder/v1/upstreams", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"upstreams": []map[string]any{{"upstream": "u"}}})
	})
	mux.HandleFunc("/_larder/v1/offline", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Offline bool `json:"offline"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		offline = body.Offline
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/_larder/v1/reload", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"reloaded": true})
	})
	mux.HandleFunc("/_larder/v1/jobs/abc", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "abc", "state": "done"})
	})
	mux.HandleFunc("/bad", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found", "message": "missing"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := client.Stats(context.Background())
	if err != nil || stats["used_bytes"].(float64) != 7 {
		t.Fatalf("stats = %#v, %v", stats, err)
	}
	upstreams, err := client.Upstreams(context.Background())
	if err != nil || len(upstreams) != 1 {
		t.Fatalf("upstreams = %#v, %v", upstreams, err)
	}
	if err := client.SetOffline(context.Background(), true); err != nil || !offline {
		t.Fatalf("offline = %v, %v", offline, err)
	}
	if err := client.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := client.Job(context.Background(), "abc")
	if err != nil || job["id"] != "abc" {
		t.Fatalf("job = %#v, %v", job, err)
	}
	err = client.Get(context.Background(), "/bad", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.Code != "not_found" {
		t.Fatalf("api error = %#v", err)
	}
}

func TestClientErrors(t *testing.T) {
	if _, err := New("", 0); err == nil {
		t.Fatal("empty base accepted")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	client, err := New("http://"+addr, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), "/", nil); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("unreachable error = %v", err)
	}
}
