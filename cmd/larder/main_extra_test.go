package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestUsageAndFlagErrors(t *testing.T) {
	tests := [][]string{
		{},
		{"-badflag"},
		{"config"},
		{"config", "nope"},
		{"config", "check", "extra"},
		{"config", "print", "extra"},
		{"config", "defaults", "extra"},
		{"version", "extra"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"version", "-h"}, {"config", "check", "-h"}, {"config", "print", "-h"}, {"config", "defaults", "-h"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Errorf("run(%v) = %d, want 0", args, code)
		}
	}
}

func TestConfigPrintLoadFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-c", "/does/not/exist", "config", "print"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestWriteFailures(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"--json", "version"}, failingWriter{}, &stderr); code != 1 {
		t.Fatalf("json version write failure exit = %d", code)
	}
	stderr.Reset()
	if code := run([]string{"config", "defaults"}, failingWriter{}, &stderr); code != 1 {
		t.Fatalf("defaults write failure exit = %d", code)
	}
	stderr.Reset()
	path := writeTestConfig(t, "")
	if code := run([]string{"-c", path, "config", "print"}, failingWriter{}, &stderr); code != 1 {
		t.Fatalf("print write failure exit = %d", code)
	}
}

func TestConnectWriteFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	path := writeTestConfig(t, "repos:\n  - name: generic\n    kind: generic\n    upstreams:\n      - url: "+server.URL+"\n")
	var stderr bytes.Buffer
	if code := run([]string{"-c", path, "config", "check", "--connect"}, failingWriter{}, &stderr); code != 1 {
		t.Fatalf("connect write failure exit = %d", code)
	}
}
