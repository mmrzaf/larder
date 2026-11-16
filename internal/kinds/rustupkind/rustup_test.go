package rustupkind

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/kindtest"
	"github.com/mmrzaf/larder/testutil"
)

func newKind(t *testing.T) kinds.Kind {
	t.Helper()
	kind, err := New(config.Repo{
		Name: "rustup", Kind: "rustup", Mount: "/rustup",
		Upstreams: []config.Upstream{{URL: "https://static.rust-lang.org"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

func TestMatrix_R06_Classification(t *testing.T) {
	kind := newKind(t)
	kindtest.Table(t, kind, []kindtest.Case{
		{Name: "channel", Path: "/dist/channel-rust-stable.toml", Want: kinds.Plan{Class: kinds.Mutable, Key: "/dist/channel-rust-stable.toml", UpstreamPath: "/dist/channel-rust-stable.toml", FreshTTL: 300 * time.Second}},
		{Name: "channel checksum", Path: "/dist/channel-rust-stable.toml.sha256", Want: kinds.Plan{Class: kinds.Mutable, Key: "/dist/channel-rust-stable.toml.sha256", UpstreamPath: "/dist/channel-rust-stable.toml.sha256", FreshTTL: 300 * time.Second}},
		{Name: "channel signature", Path: "/dist/channel-rust-stable.toml.asc", Want: kinds.Plan{Class: kinds.Mutable, Key: "/dist/channel-rust-stable.toml.asc", UpstreamPath: "/dist/channel-rust-stable.toml.asc", FreshTTL: 300 * time.Second, Binary: true}},
		{Name: "version channel", Path: "/dist/channel-rust-1.75.0.toml", Want: kinds.Plan{Class: kinds.Mutable, Key: "/dist/channel-rust-1.75.0.toml", UpstreamPath: "/dist/channel-rust-1.75.0.toml", FreshTTL: 300 * time.Second}},
		{Name: "dated artifact", Path: "/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.xz", Want: kinds.Plan{Class: kinds.Immutable, Key: "/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.xz", UpstreamPath: "/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.xz", Binary: true}},
		{Name: "dated manifest", Path: "/dist/2024-01-01/channel-rust-stable.toml", Want: kinds.Plan{Class: kinds.Immutable, Key: "/dist/2024-01-01/channel-rust-stable.toml", UpstreamPath: "/dist/2024-01-01/channel-rust-stable.toml"}},
		{Name: "invalid date", Path: "/dist/2024-1-1/rustc.tar.xz", Want: kinds.Plan{Class: kinds.Deny, Key: "/dist/2024-1-1/rustc.tar.xz", UpstreamPath: "/dist/2024-1-1/rustc.tar.xz"}},
		{Name: "dated directory", Path: "/dist/2024-01-01/", Want: kinds.Plan{Class: kinds.Deny, Key: "/dist/2024-01-01/", UpstreamPath: "/dist/2024-01-01/"}},
		{Name: "rustup init", Path: "/rustup/dist/x86_64-unknown-linux-gnu/rustup-init", Want: kinds.Plan{Class: kinds.Mutable, Key: "/rustup/dist/x86_64-unknown-linux-gnu/rustup-init", UpstreamPath: "/rustup/dist/x86_64-unknown-linux-gnu/rustup-init", FreshTTL: 300 * time.Second, Binary: true}},
		{Name: "rustup init checksum", Path: "/rustup/dist/x86_64-unknown-linux-gnu/rustup-init.sha256", Want: kinds.Plan{Class: kinds.Mutable, Key: "/rustup/dist/x86_64-unknown-linux-gnu/rustup-init.sha256", UpstreamPath: "/rustup/dist/x86_64-unknown-linux-gnu/rustup-init.sha256", FreshTTL: 300 * time.Second}},
		{Name: "archive", Path: "/rustup/archive/1.75.0/x86_64-unknown-linux-gnu/rustup-init.exe", Want: kinds.Plan{Class: kinds.Immutable, Key: "/rustup/archive/1.75.0/x86_64-unknown-linux-gnu/rustup-init.exe", UpstreamPath: "/rustup/archive/1.75.0/x86_64-unknown-linux-gnu/rustup-init.exe", Binary: true}},
		{Name: "other rustup path", Path: "/rustup/other", Want: kinds.Plan{Class: kinds.Deny, Key: "/rustup/other", UpstreamPath: "/rustup/other"}},
		{Name: "channel prefix only", Path: "/dist/channel-rust-stable.toml2", Want: kinds.Plan{Class: kinds.Deny, Key: "/dist/channel-rust-stable.toml2", UpstreamPath: "/dist/channel-rust-stable.toml2"}},
	})
}

func TestManifestIngest(t *testing.T) {
	kind := newKind(t)
	plan, err := kind.Classify(kinds.Request{Path: "/dist/channel-rust-stable.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Hooks.Ingest == nil {
		t.Fatal("manifest ingest hook missing")
	}
	body := strings.ReplaceAll(string(testutil.Fixture(t, "rustup", "channel-rust-stable.toml")), "HASH_TAR", strings.Repeat("a", 64))
	body = strings.ReplaceAll(body, "HASH_XZ", strings.Repeat("b", 64))
	sink := &recordingSink{}
	if err := plan.Hooks.Ingest(context.Background(), kinds.ObjectRef{Repo: "rustup", Key: plan.Key}, strings.NewReader(body), sink); err != nil {
		t.Fatal(err)
	}
	if sink.expectations["/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.gz"] != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("tar expectation = %#v", sink.expectations)
	}
	if sink.expectations["/dist/2024-01-01/rustc-1.75.0-x86_64-unknown-linux-gnu.tar.xz"] != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("xz expectation = %#v", sink.expectations)
	}

	checksumPlan, err := kind.Classify(kinds.Request{Path: "/dist/2024-01-01/rustc.tar.xz.sha256"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, algo, ok := checksumPlan.Hooks.ChecksumFor(checksumPlan.Key)
	if !ok || artifact != "/dist/2024-01-01/rustc.tar.xz" || algo != "sha256" {
		t.Fatalf("checksum hook = %q %q %v", artifact, algo, ok)
	}
}

func TestManifestArtifactKeyWithMirrorBase(t *testing.T) {
	kind, err := New(config.Repo{
		Name: "rustup", Kind: "rustup", Mount: "/rustup",
		Upstreams: []config.Upstream{{URL: "https://mirror.example/rustup"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	impl := kind.(*Kind)
	key, ok := impl.artifactKey("https://mirror.example/rustup/dist/2024-01-01/rustc.tar.xz")
	if !ok || key != "/dist/2024-01-01/rustc.tar.xz" {
		t.Fatalf("artifact key = %q, %v", key, ok)
	}
}

type recordingSink struct {
	expectations map[string]string
	stale        []string
}

func (s *recordingSink) Expect(key, algo, digest string) {
	if s.expectations == nil {
		s.expectations = make(map[string]string)
	}
	s.expectations[key] = algo + ":" + digest
}

func (s *recordingSink) MarkStale(pattern string) { s.stale = append(s.stale, pattern) }
func (s *recordingSink) Logger() *slog.Logger     { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func FuzzManifestIngest(f *testing.F) {
	f.Add([]byte("manifest-version = \"2\"\n[pkg.rustc.target.x86_64-unknown-linux-gnu]\nhash = \"a\"\n"))
	f.Add([]byte(""))
	f.Add([]byte("[pkg"))
	f.Fuzz(func(t *testing.T, body []byte) {
		kind := newKind(t).(*Kind)
		_ = kind.ingest(context.Background(), kinds.ObjectRef{Repo: "rustup", Key: "/dist/channel-rust-stable.toml"}, bytes.NewReader(body), &recordingSink{})
	})
}
