package main

import (
	"bytes"
	"encoding/json"
	"github.com/mmrzaf/larder/internal/store"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "larder.yaml")
	dir := t.TempDir()
	content := "version: 1\ndata_dir: " + dir + "\n" + body
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMatrix_L01_ConfigCheckAllErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := `
version: 1
data_dir: ` + dir + `
storage:
  low_watermark: 0.95
  high_watermark: 0.90
  hard_free_bytes: 2GiB
  min_free_bytes: 1GiB
offline_miss_status: 418
log:
  levl: info
repos:
  - name: one
    kind: generic
    mount: /maven
    upstreams:
      - url: ftp://bad
    rules:
      - {match: '[', class: immutable}
  - name: one
    kind: generic
    mount: /maven/central
    upstreams:
      - url: https://example.com
`
	path := filepath.Join(t.TempDir(), "larder.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o640); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-c", path, "config", "check"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"log.levl",
		"storage.low_watermark",
		"storage.hard_free_bytes",
		"offline_miss_status",
		"repos[0].upstreams[0].url",
		"repos[0].rules[0].match",
		"duplicate repo name",
		"overlaps",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not contain %q:\n%s", want, stderr.String())
		}
	}
	if stdout.Len() != 0 {
		t.Errorf("config check wrote to stdout: %s", stdout.String())
	}
}

func TestMatrix_L02_ConfigPrintRedactsSecrets(t *testing.T) {
	path := writeTestConfig(t, `
upstream_proxy: http://proxyuser:proxypass@127.0.0.1:3128
repos:
  - name: generic
    kind: generic
    proxy: socks5h://alice:secret@127.0.0.1:1080
    upstreams:
      - url: https://user:upstreampass@example.com/base
        headers:
          Authorization: Bearer topsecret
          X-Api-Key: another-secret
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-c", path, "config", "print"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d\nstderr:\n%s", code, stderr.String())
	}
	for _, secret := range []string{"proxypass", "proxyuser", "secret", "alice", "upstreampass", "topsecret", "another-secret"} {
		if strings.Contains(stdout.String(), secret) {
			t.Errorf("printed configuration leaked %q:\n%s", secret, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), "***") {
		t.Errorf("printed configuration is not redacted:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Authorization") {
		t.Errorf("header name was removed:\n%s", stdout.String())
	}
}

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "larder dev") {
		t.Fatalf("version output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--json", "version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("json exit code = %d: %s", code, stderr.String())
	}
	var info map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "commit", "date", "go_version"} {
		if info[key] == "" {
			t.Errorf("json version missing %s: %#v", key, info)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"unknown"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage: larder") {
		t.Fatalf("usage missing:\n%s", stderr.String())
	}
}

func TestConfigDefaults(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "defaults"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "version: 1") || !strings.Contains(stdout.String(), "repos: []") {
		t.Fatalf("unexpected defaults:\n%s", stdout.String())
	}
}

func TestConfigCheckConnect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	path := writeTestConfig(t, "repos:\n  - name: generic\n    kind: generic\n    upstreams:\n      - url: "+server.URL+"\n")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-c", path, "config", "check", "--connect"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d\nstderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "reachable") {
		t.Fatalf("connect output = %q", stdout.String())
	}
}

func TestConfigCheckConnectUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	path := writeTestConfig(t, "repos:\n  - name: generic\n    kind: generic\n    upstreams:\n      - url: http://"+addr+"\n")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-c", path, "config", "check", "--connect"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "unreachable") {
		t.Fatalf("connect output = %q", stdout.String())
	}
}

func TestMatrix_L17_Healthcheck(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"healthcheck", "--url", healthy.URL + "/readyz"}, &stdout, &stderr); code != 0 {
		t.Fatalf("healthy exit = %d: %s", code, stderr.String())
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"healthcheck", "--url", unhealthy.URL + "/readyz"}, &stdout, &stderr); code != 1 {
		t.Fatalf("unhealthy exit = %d", code)
	}
}

func TestServeErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-c", "/does/not/exist", "serve"}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing config exit = %d", code)
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "larder.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 1\ndata_dir: "+dir+"\nrepos: []\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	locked, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locked.Close() }()
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-c", cfgPath, "serve"}, &stdout, &stderr); code != 4 {
		t.Fatalf("locked data dir exit = %d: %s", code, stderr.String())
	}

	badListen := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(badListen, []byte("version: 1\ndata_dir: "+t.TempDir()+"\nlisten: not-an-address\nrepos: []\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-c", badListen, "serve"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("malformed listen address exit = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "listen:") {
		t.Fatalf("diagnostic does not name the listen key: %s", stderr.String())
	}

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	inUse := filepath.Join(t.TempDir(), "inuse.yaml")
	if err := os.WriteFile(inUse, []byte("version: 1\ndata_dir: "+t.TempDir()+"\nlisten: "+occupied.Addr().String()+"\nrepos: []\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-c", inUse, "serve"}, &stdout, &stderr); code != 1 {
		t.Fatalf("listen address in use exit = %d: %s", code, stderr.String())
	}
}
