package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// FixturePath returns the absolute path of a file under testutil/fixtures.
func FixturePath(t testing.TB, parts ...string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate testutil package")
	}
	all := append([]string{filepath.Dir(file), "fixtures"}, parts...)
	return filepath.Join(all...)
}

// Fixture reads a file under testutil/fixtures.
func Fixture(t testing.TB, parts ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(FixturePath(t, parts...))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
