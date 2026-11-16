package gokind

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/sumdb/dirhash"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/kindtest"
)

func newKind(t *testing.T, opts config.GoOptions) kinds.Kind {
	t.Helper()
	kind, err := New(config.Repo{Name: "go", Kind: "go", Go: &opts})
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

func TestMatrix_G01_Classification(t *testing.T) {
	kind := newKind(t, config.GoOptions{Verify: "basic"})
	kindtest.Table(t, kind, []kindtest.Case{
		{Path: "/example.com/mod/@v/list", Want: kinds.Plan{Class: kinds.Mutable, Key: "/example.com/mod/@v/list", UpstreamPath: "/example.com/mod/@v/list", FreshTTL: 60 * time.Second, Binary: true}},
		{Path: "/example.com/mod/@latest", Want: kinds.Plan{Class: kinds.Mutable, Key: "/example.com/mod/@latest", UpstreamPath: "/example.com/mod/@latest", FreshTTL: 60 * time.Second, Binary: true}},
		{Path: "/example.com/mod/@v/v1.2.3.info", Want: kinds.Plan{Class: kinds.Immutable, Key: "/example.com/mod/@v/v1.2.3.info", UpstreamPath: "/example.com/mod/@v/v1.2.3.info", Binary: true}},
		{Path: "/example.com/mod/@v/master.info", Want: kinds.Plan{Class: kinds.Mutable, Key: "/example.com/mod/@v/master.info", UpstreamPath: "/example.com/mod/@v/master.info", FreshTTL: 60 * time.Second, Binary: true}},
		{Path: "/example.com/mod/@v/v1.2.3.mod", Want: kinds.Plan{Class: kinds.Immutable, Key: "/example.com/mod/@v/v1.2.3.mod", UpstreamPath: "/example.com/mod/@v/v1.2.3.mod", Binary: true}},
		{Path: "/example.com/mod/@v/v1.2.3.zip", Want: kinds.Plan{Class: kinds.Immutable, Key: "/example.com/mod/@v/v1.2.3.zip", UpstreamPath: "/example.com/mod/@v/v1.2.3.zip", Binary: true}},
		{Path: "/example.com/mod/@v/master.mod", Want: kinds.Plan{Class: kinds.Deny, Key: "/example.com/mod/@v/master.mod", UpstreamPath: "/example.com/mod/@v/master.mod"}},
		{Path: "/example.com/mod/@v/master.zip", Want: kinds.Plan{Class: kinds.Deny, Key: "/example.com/mod/@v/master.zip", UpstreamPath: "/example.com/mod/@v/master.zip"}},
		{Name: "deny", Path: "/example.com/mod/other", Want: kinds.Plan{Class: kinds.Deny, Key: "/example.com/mod/other", UpstreamPath: "/example.com/mod/other"}},
		{Name: "case encoded", Path: "/example.com/!mod/@v/v1.0.0.zip", Want: kinds.Plan{Class: kinds.Immutable, Key: "/example.com/!mod/@v/v1.0.0.zip", UpstreamPath: "/example.com/!mod/@v/v1.0.0.zip", Binary: true}},
		{Name: "invalid module", Path: "/bad path/@v/list", WantErr: 400},
		{Name: "sumdb supported", Path: "/sumdb/sum.golang.org/supported", Want: kinds.Plan{Class: kinds.Mutable, Key: "/sumdb/sum.golang.org/supported", UpstreamPath: "/sumdb/sum.golang.org/supported", FreshTTL: time.Hour}},
		{Name: "sumdb latest", Path: "/sumdb/sum.golang.org/latest", Want: kinds.Plan{Class: kinds.Mutable, Key: "/sumdb/sum.golang.org/latest", UpstreamPath: "/sumdb/sum.golang.org/latest", FreshTTL: 60 * time.Second}},
		{Name: "sumdb lookup", Path: "/sumdb/sum.golang.org/lookup/example.com/mod@v1.2.3", Want: kinds.Plan{Class: kinds.Immutable, Key: "/sumdb/sum.golang.org/lookup/example.com/mod@v1.2.3", UpstreamPath: "/sumdb/sum.golang.org/lookup/example.com/mod@v1.2.3", Binary: true}},
		{Name: "sumdb tile", Path: "/sumdb/sum.golang.org/tile/8/0/000", Want: kinds.Plan{Class: kinds.Immutable, Key: "/sumdb/sum.golang.org/tile/8/0/000", UpstreamPath: "/sumdb/sum.golang.org/tile/8/0/000", Binary: true}},
		{Name: "unknown sumdb", Path: "/sumdb/other.example/supported", Want: kinds.Plan{Class: kinds.Deny, Key: "/sumdb/other.example/supported", UpstreamPath: "/sumdb/other.example/supported"}},
		{Name: "unknown sumdb path", Path: "/sumdb/sum.golang.org/nope", Want: kinds.Plan{Class: kinds.Deny, Key: "/sumdb/sum.golang.org/nope", UpstreamPath: "/sumdb/sum.golang.org/nope"}},
	})
}

func TestGoSumdbResolve(t *testing.T) {
	configured := newKind(t, config.GoOptions{
		SumDBUpstreams: map[string][]string{"sum.golang.org": {"https://sum.example"}},
	})
	plan, err := configured.Classify(kinds.Request{Path: "/sumdb/sum.golang.org/lookup/example.com/mod@v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := plan.Hooks.Resolve([]kinds.Upstream{{URL: "https://proxy.example"}})
	if err != nil || len(candidates) != 1 || candidates[0].URL != "https://sum.example/lookup/example.com/mod@v1.0.0" {
		t.Fatalf("configured candidates = %#v, %v", candidates, err)
	}

	derived := newKind(t, config.GoOptions{})
	plan, err = derived.Classify(kinds.Request{Path: "/sumdb/sum.golang.org/latest"})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err = plan.Hooks.Resolve([]kinds.Upstream{{URL: "https://proxy.example/"}})
	if err != nil || len(candidates) != 2 {
		t.Fatalf("derived candidates = %#v, %v", candidates, err)
	}
	if candidates[0].URL != "https://proxy.example/sumdb/sum.golang.org/latest" || candidates[1].URL != "https://sum.golang.org/latest" {
		t.Fatalf("derived candidates = %#v", candidates)
	}
}

func TestGoValidators(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "v1.2.3.mod")
	if err := os.WriteFile(modPath, []byte("module example.com/mod\n\ngo 1.23\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := modValidator(context.Background(), openFile(t, modPath), fileSize(t, modPath)); err != nil {
		t.Fatal(err)
	}
	badMod := filepath.Join(dir, "bad.mod")
	if err := os.WriteFile(badMod, []byte{0xff, 0xfe}, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := modValidator(context.Background(), openFile(t, badMod), fileSize(t, badMod)); err == nil {
		t.Fatal("invalid UTF-8 mod accepted")
	}

	infoPath := filepath.Join(dir, "v1.2.3.info")
	if err := os.WriteFile(infoPath, []byte(`{"Version":"v1.2.3"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := infoValidator(context.Background(), openFile(t, infoPath), fileSize(t, infoPath)); err != nil {
		t.Fatal(err)
	}
	badInfo := filepath.Join(dir, "bad.info")
	if err := os.WriteFile(badInfo, []byte(`{"Version":""}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := infoValidator(context.Background(), openFile(t, badInfo), fileSize(t, badInfo)); err == nil {
		t.Fatal("invalid info accepted")
	}

	zipPath := filepath.Join(dir, "v1.2.3.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("example.com/!mod@v1.2.3/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("module example.com/Mod\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipValidator("example.com/!mod@v1.2.3/")(context.Background(), openFile(t, zipPath), fileSize(t, zipPath)); err != nil {
		t.Fatal(err)
	}
	badZip := filepath.Join(dir, "bad.zip")
	if err := os.WriteFile(badZip, []byte("not a zip"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := zipValidator("example.com/mod@v1.0.0/")(context.Background(), openFile(t, badZip), fileSize(t, badZip)); err == nil {
		t.Fatal("invalid zip accepted")
	}
}

func openFile(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestGoInfoValidatorHook(t *testing.T) {
	kind := newKind(t, config.GoOptions{Verify: "strict"})
	plan, err := kind.Classify(kinds.Request{Path: "/example.com/mod/@v/v1.2.3.info"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Hooks.Validate == nil {
		t.Fatal("info validator missing")
	}
}

func TestGoConstructorError(t *testing.T) {
	if _, err := New(config.Repo{Name: "go", Kind: "go"}); err == nil {
		t.Fatal("missing go options accepted")
	}
}

func TestStrictVerifier(t *testing.T) {
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	entry, err := writer.Create("example.com/hello@v1.0.0/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mod := []byte("module example.com/hello\n\ngo 1.23\n")
	if _, err := entry.Write(mod); err != nil {
		t.Fatal(err)
	}
	entry, err = writer.Create("example.com/hello@v1.0.0/hello.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("package hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()
	path := filepath.Join(t.TempDir(), "module.zip")
	if err := os.WriteFile(path, archive, 0o640); err != nil {
		t.Fatal(err)
	}
	zipHash, err := dirhash.HashZip(path, dirhash.Hash1)
	if err != nil {
		t.Fatal(err)
	}
	zipHash = strings.TrimPrefix(zipHash, "h1:")
	modHash, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(mod)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	modHash = strings.TrimPrefix(modHash, "h1:")
	lookup := []byte("123\n" +
		"example.com/hello v1.0.0 h1:" + zipHash + "\n" +
		"example.com/hello v1.0.0/go.mod h1:" + modHash + "\n")
	fetch := func(context.Context, string) ([]byte, error) { return lookup, nil }

	zipVerifier := strictVerifier("example.com/hello", "v1.0.0", "v1.0.0", true)
	digest, err := zipVerifier(context.Background(), bytes.NewReader(archive), int64(len(archive)), fetch)
	if err != nil || digest != zipHash {
		t.Fatalf("zip verification = %q, %v; want %q", digest, err, zipHash)
	}
	modVerifier := strictVerifier("example.com/hello", "v1.0.0", "v1.0.0", false)
	digest, err = modVerifier(context.Background(), bytes.NewReader(mod), int64(len(mod)), fetch)
	if err != nil || digest != modHash {
		t.Fatalf("mod verification = %q, %v; want %q", digest, err, modHash)
	}

	wrong := []byte("123\nexample.com/hello v1.0.0 h1:AAAA\n")
	if _, err := zipVerifier(context.Background(), bytes.NewReader(archive), int64(len(archive)), func(context.Context, string) ([]byte, error) {
		return wrong, nil
	}); err == nil || errors.Is(err, kinds.ErrVerificationUnavailable) {
		t.Fatalf("wrong hash error = %v", err)
	}

	if _, err := zipVerifier(context.Background(), bytes.NewReader(archive), int64(len(archive)), func(context.Context, string) ([]byte, error) {
		return nil, errors.New("offline")
	}); !errors.Is(err, kinds.ErrVerificationUnavailable) {
		t.Fatalf("lookup failure = %v", err)
	}
	if _, err := zipVerifier(context.Background(), bytes.NewReader([]byte("not a zip")), 10, fetch); err == nil || errors.Is(err, kinds.ErrVerificationUnavailable) {
		t.Fatalf("invalid zip hash error = %v", err)
	}
	if _, err := modVerifier(context.Background(), bytes.NewReader(mod[:10]), 10, fetch); err == nil || errors.Is(err, kinds.ErrVerificationUnavailable) {
		t.Fatalf("short mod hash error = %v", err)
	}
	if _, err := modVerifier(context.Background(), bytes.NewReader(mod), int64(len(mod)), func(context.Context, string) ([]byte, error) {
		return []byte("123\nexample.com/hello v1.0.0 zip\n"), nil
	}); !errors.Is(err, kinds.ErrVerificationUnavailable) {
		t.Fatalf("missing go.mod hash = %v", err)
	}
}

func TestName(t *testing.T) {
	if got := newKind(t, config.GoOptions{}).Name(); got != "go" {
		t.Fatalf("Name() = %q", got)
	}
}
