package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TempDataDir returns an empty data directory owned by the test.
func TempDataDir(t testing.TB) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create temp data directory: %v", err)
	}
	return dir
}
