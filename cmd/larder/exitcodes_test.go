package main

import (
	"path/filepath"
	"testing"
)

func TestMatrix_L16_ExitCodes(t *testing.T) {
	if code, _, _ := runCLI(t, "version"); code != 0 {
		t.Fatalf("version exit %d", code)
	}
	if code, _, _ := runCLI(t, "not-a-command"); code != 2 {
		t.Fatalf("unknown command exit %d", code)
	}
	if code, _, _ := runCLI(t, "-c", filepath.Join(t.TempDir(), "missing.yaml"), "config", "check"); code != 2 {
		t.Fatalf("missing configuration exit %d", code)
	}
	f := newCLIFixture(t, 1)
	f.close()
	if code, _, _ := runCLI(t, "-c", f.path, "offline", "status"); code != 3 {
		t.Fatalf("offline without admin exit %d", code)
	}
	if code, _, _ := runCLI(t, "-c", f.path, "import", "--from", filepath.Join(t.TempDir(), "missing")); code != 1 {
		t.Fatalf("failed import exit %d", code)
	}
	ts := startCLIServer(t, nil)
	if code, _, _ := runCLI(t, "-c", ts.path, "reindex", "--fix"); code != 4 {
		t.Fatalf("reindex with the server running exit %d, want 4", code)
	}
}
