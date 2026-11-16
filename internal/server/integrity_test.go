package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/testutil"
)

func TestMatrix_S16_VerifyRepairThroughAdmin(t *testing.T) {
	ts := newTestServer(t, genericImmutableRepo(), nil)
	body := bytes.Repeat([]byte("repair me "), 512)
	ts.upstream.Set("/file", testutil.Route{Body: body})
	response, err := http.Get(ts.publicURL("/generic/file"))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Equal(stored, body) {
		t.Fatalf("initial fetch status %d", response.StatusCode)
	}
	object, err := ts.index.CurrentObject(context.Background(), "generic", "/file")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ts.cfg.DataDir, "blobs", object.SHA256[:2], object.SHA256[2:4], object.SHA256)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(body)), 0o640); err != nil {
		t.Fatal(err)
	}

	requestBody, _ := json.Marshal(map[string]any{"repair": true, "sample": 100})
	response, err = http.Post(ts.adminURL("/_larder/v1/verify"), "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		Job string `json:"job"`
	}
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("verify status = %d", response.StatusCode)
	}
	deadline := time.Now().Add(10 * time.Second)
	var result struct {
		State  string `json:"state"`
		Result struct {
			Corrupt     int64 `json:"corrupt"`
			Repaired    int64 `json:"repaired"`
			Quarantined int64 `json:"quarantined"`
		} `json:"result"`
	}
	for {
		response, err := http.Get(ts.adminURL("/_larder/v1/jobs/" + accepted.Job))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if result.State == "done" {
			break
		}
		if result.State == "failed" {
			t.Fatalf("verify job failed: %+v", result)
		}
		if time.Now().After(deadline) {
			t.Fatal("verify job did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if result.Result.Corrupt != 1 || result.Result.Repaired != 1 || result.Result.Quarantined != 1 {
		t.Fatalf("verify result = %+v", result.Result)
	}
	response, err = http.Get(ts.publicURL("/generic/file"))
	if err != nil {
		t.Fatal(err)
	}
	repaired, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Equal(repaired, body) {
		t.Fatalf("repaired fetch status %d body %d bytes", response.StatusCode, len(repaired))
	}
}

func TestRepairReferencesOffline(t *testing.T) {
	ts := newTestServer(t, genericImmutableRepo(), nil)
	runtime := ts.server.acquire()
	runtime.engine.SetOffline(true)
	ts.server.release(runtime)
	repaired, err := ts.server.repairReferences(context.Background(), nil)
	if err != nil || repaired != 0 {
		t.Fatalf("offline repair = %d, %v", repaired, err)
	}
}

func genericImmutableRepo() config.Repo {
	return config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{
			DefaultClass: "deny", RejectHTML: true,
			Rules: []config.GenericRule{{Match: `^/file$`, Class: "immutable"}},
		},
	}
}

func TestAdminExportJob(t *testing.T) {
	ts := newTestServer(t, genericImmutableRepo(), nil)
	ts.upstream.Set("/file", testutil.Route{Body: []byte("export through admin")})
	response, err := http.Get(ts.publicURL("/generic/file"))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	destination := filepath.Join(t.TempDir(), "admin-export")
	requestBody, _ := json.Marshal(map[string]any{"to": destination, "format": "tree"})
	response, err = http.Post(ts.adminURL("/_larder/v1/export"), "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		Job string `json:"job"`
	}
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("export status = %d", response.StatusCode)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := http.Get(ts.adminURL("/_larder/v1/jobs/" + accepted.Job))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if document.State == "done" {
			break
		}
		if document.State == "failed" {
			t.Fatal("export job failed")
		}
		if time.Now().After(deadline) {
			t.Fatal("export job did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(destination, "larder-export.json")); err != nil {
		t.Fatal(err)
	}
}
