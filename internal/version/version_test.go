package version

import (
	"runtime"
	"testing"
)

func TestGet(t *testing.T) {
	info := Get()
	if info.Version == "" || info.Commit == "" || info.Date == "" {
		t.Fatalf("incomplete build info: %#v", info)
	}
	if info.GoVersion != runtime.Version() {
		t.Fatalf("Go version = %q, want %q", info.GoVersion, runtime.Version())
	}
}
