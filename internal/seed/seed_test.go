package seed

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mmrzaf/larder/internal/warm"
)

func TestLoadStrictSeedFiles(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.yaml")
	if err := os.WriteFile(valid, []byte(`version: 1
pin_set: baseline
go:
  modules: [example.com/mod@v1.2.3]
npm:
  packages: ["typescript@5.6.2"]
pypi:
  packages: ["requests>=2.32"]
  only_binary: true
crates:
  packages: ["serde@1"]
rustup:
  channels: [stable]
  targets: [x86_64-unknown-linux-gnu]
  components: [rustc]
maven:
  repo: maven-central
  coordinates: ["g:a:1.0"]
  transitive: true
gradle_dist:
  repo: gradle-dist
  versions: ["8.10.2"]
apt:
  repo: apt-debian
  suite: bookworm
  arch: [amd64]
  packages: [git]
generic:
  - repo: github
    paths: [/owner/tool]
`), 0o640); err != nil {
		t.Fatal(err)
	}
	file, err := Load(valid)
	if err != nil {
		t.Fatalf("valid seed: %v", err)
	}
	if file.PinSet != "baseline" || file.Go == nil || file.Apt == nil || len(file.Generic) != 1 {
		t.Fatalf("parsed file = %+v", file)
	}
	unknown := filepath.Join(dir, "unknown.yaml")
	if err := os.WriteFile(unknown, []byte("version: 1\nbogus: true\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unknown); err == nil {
		t.Fatal("unknown key accepted")
	}
	version := filepath.Join(dir, "version.yaml")
	if err := os.WriteFile(version, []byte("version: 2\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(version); err == nil {
		t.Fatal("version 2 accepted")
	}
	empty := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(empty, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(empty); err == nil {
		t.Fatal("empty file accepted")
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestResolveAptClosure(t *testing.T) {
	packages := `Package: app
Version: 1.0
Depends: lib (>= 1.0), virtual
Filename: pool/main/a/app/app_1.0_amd64.deb

Package: lib
Version: 1.2
Filename: pool/main/l/lib/lib_1.2_amd64.deb

Package: provider
Version: 2.0
Provides: virtual
Filename: pool/main/p/provider/provider_2.0_amd64.deb
`
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(packages)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apt/dists/bookworm/main/binary-amd64/Packages.gz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(compressed.Bytes())
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	file := &File{Apt: &AptSection{
		Repo: "apt", Suite: "bookworm", Arch: []string{"amd64"}, Packages: []string{"app"},
	}}
	result, err := Resolve(context.Background(), file, Options{
		BaseURL: server.URL,
		Repos:   []warm.Repo{{Name: "apt", Kind: "apt", Mount: "/apt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sections) != 1 || result.Sections[0].Failed {
		t.Fatalf("sections = %+v", result.Sections)
	}
	joined := strings.Join(result.Sections[0].Resolved, ",")
	for _, want := range []string{"app=1.0", "lib=1.2", "provider=2.0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("resolved = %s, missing %s", joined, want)
		}
	}
	paths := ""
	for _, item := range result.Items {
		paths += item.Path + "\n"
	}
	for _, want := range []string{
		"/dists/bookworm/InRelease", "/dists/bookworm/main/binary-amd64/Packages.gz",
		"/pool/main/a/app/app_1.0_amd64.deb", "/pool/main/l/lib/lib_1.2_amd64.deb",
		"/pool/main/p/provider/provider_2.0_amd64.deb",
	} {
		if !strings.Contains(paths, want) {
			t.Fatalf("items = %s, missing %s", paths, want)
		}
	}
}

func TestResolveRustupManifest(t *testing.T) {
	manifest := `manifest-version = "2"
date = "2024-01-01"
[pkg.rustc.target.x86_64-unknown-linux-gnu]
hash = "HASH_TAR"
xz_hash = "HASH_XZ"
url = "https://static.rust-lang.org/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.gz"
xz_url = "https://static.rust-lang.org/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.xz"
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rustup/dist/channel-rust-stable.toml" {
			_, _ = w.Write([]byte(manifest))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	file := &File{Rustup: &RustupSection{
		Channels: []string{"stable"}, Targets: []string{"x86_64-unknown-linux-gnu"},
		Components: []string{"rustc"},
	}}
	result, err := Resolve(context.Background(), file, Options{
		BaseURL: server.URL,
		Repos:   []warm.Repo{{Name: "rustup", Kind: "rustup", Mount: "/rustup"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := ""
	for _, item := range result.Items {
		paths += item.Path + "\n"
	}
	if !strings.Contains(paths, "/dist/channel-rust-stable.toml") ||
		!strings.Contains(paths, "/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.xz") {
		t.Fatalf("rustup items = %s", paths)
	}
}
