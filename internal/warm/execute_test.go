package warm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestExecuteRetriesAndCacheCounting(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repo/a":
			if attempts.Add(1) == 1 {
				http.Error(w, "retry", http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Larder-Cache", "MISS")
			_, _ = w.Write([]byte("fetched"))
		case "/repo/b":
			w.Header().Set("X-Larder-Cache", "HIT")
			_, _ = w.Write([]byte("cached"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	plan := Plan{Items: []Item{
		{Repo: "repo", Path: "/a"},
		{Repo: "repo", Path: "/b"},
	}}
	summary := Execute(context.Background(), plan, ExecuteOptions{
		BaseURL: server.URL, Mounts: map[string]string{"repo": "/repo"},
		Concurrency: 2, Retries: 1,
	})
	if summary.Failed != 0 || summary.Fetched != 1 || summary.Cached != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Bytes != int64(len("fetched")) {
		t.Fatalf("bytes = %d", summary.Bytes)
	}
}

func TestExecuteFailuresAndCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer server.Close()
	plan := Plan{Items: []Item{{Repo: "repo", Path: "/missing"}}}
	summary := Execute(context.Background(), plan, ExecuteOptions{
		BaseURL: server.URL, Mounts: map[string]string{"repo": "/repo"}, Retries: 2,
	})
	if summary.Failed != 1 || len(summary.Results) != 1 || summary.Results[0].Status != http.StatusNotFound {
		t.Fatalf("summary = %+v", summary)
	}
	empty := Execute(context.Background(), plan, ExecuteOptions{})
	if empty.Failed != 1 {
		t.Fatalf("empty base summary = %+v", empty)
	}
}
