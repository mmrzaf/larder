package cratekind

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/kindtest"
	"github.com/mmrzaf/larder/testutil"
)

func newKind(t *testing.T, templates ...string) kinds.Kind {
	t.Helper()
	if len(templates) == 0 {
		templates = []string{"https://static.crates.io/crates/{crate}/{crate}-{version}.crate"}
	}
	kind, err := New(config.Repo{
		Name: "crates", Kind: "crates", Mount: "/crates",
		Crates: &config.CratesOptions{DLUpstreams: templates},
	})
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

func TestMatrix_R01_Classification(t *testing.T) {
	kind := newKind(t)
	kindtest.Table(t, kind, []kindtest.Case{
		{Name: "config", Path: "/index/config.json", PublicBase: "https://larder.example", Want: kinds.Plan{Class: kinds.Local, Key: "/index/config.json", UpstreamPath: "/index/config.json", Local: &kinds.LocalResponse{Status: 200, Body: []byte(`{"dl":"https://larder.example/crates/dl"}`), ContentType: "application/json"}}},
		{Name: "one letter", Path: "/index/1/a", Want: kinds.Plan{Class: kinds.Mutable, Key: "/index/1/a", UpstreamPath: "/1/a"}},
		{Name: "two letters", Path: "/index/2/ab", Want: kinds.Plan{Class: kinds.Mutable, Key: "/index/2/ab", UpstreamPath: "/2/ab"}},
		{Name: "three letters", Path: "/index/3/a/abc", Want: kinds.Plan{Class: kinds.Mutable, Key: "/index/3/a/abc", UpstreamPath: "/3/a/abc"}},
		{Name: "four plus", Path: "/index/se/rd/serde", Want: kinds.Plan{Class: kinds.Mutable, Key: "/index/se/rd/serde", UpstreamPath: "/se/rd/serde"}},
		{Name: "download", Path: "/dl/serde/1.0.0/download", Want: kinds.Plan{Class: kinds.Immutable, Key: "/dl/serde/1.0.0/download", UpstreamPath: "/dl/serde/1.0.0/download", Binary: true}},
		{Name: "wrong one letter name", Path: "/index/1/ab", Want: kinds.Plan{Class: kinds.Deny, Key: "/index/1/ab", UpstreamPath: "/index/1/ab"}},
		{Name: "uppercase", Path: "/index/se/rd/Serde", Want: kinds.Plan{Class: kinds.Deny, Key: "/index/se/rd/Serde", UpstreamPath: "/index/se/rd/Serde"}},
		{Name: "wrong prefix", Path: "/index/ab/cd/serde", Want: kinds.Plan{Class: kinds.Deny, Key: "/index/ab/cd/serde", UpstreamPath: "/index/ab/cd/serde"}},
		{Name: "download extra", Path: "/dl/serde/1.0.0/file", Want: kinds.Plan{Class: kinds.Deny, Key: "/dl/serde/1.0.0/file", UpstreamPath: "/dl/serde/1.0.0/file"}},
		{Name: "unknown", Path: "/other", Want: kinds.Plan{Class: kinds.Deny, Key: "/other", UpstreamPath: "/other"}},
	})
}

func TestCrateResolveAndIngest(t *testing.T) {
	kind := newKind(t, "https://dl1.example/{crate}/{crate}-{version}.crate", "https://dl2.example/{crate}/{version}/download")
	plan, err := kind.Classify(kinds.Request{Path: "/dl/serde/1.0.0/download"})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := plan.Hooks.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].URL != "https://dl1.example/serde/serde-1.0.0.crate" || candidates[1].URL != "https://dl2.example/serde/1.0.0/download" {
		t.Fatalf("candidates = %#v", candidates)
	}

	indexPlan, err := kind.Classify(kinds.Request{Path: "/index/se/rd/serde"})
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	if err := indexPlan.Hooks.Ingest(context.Background(), kinds.ObjectRef{Repo: "crates", Key: "/index/se/rd/serde"}, bytes.NewReader(testutil.Fixture(t, "crates", "se", "rd", "serde")), sink); err != nil {
		t.Fatal(err)
	}
	expectation, ok := sink.expectations["/dl/serde/1.0.0/download"]
	if !ok || expectation != "sha256:967e0247c58f8a022bde3a80bfbd95e6af810afa4a2196efae2b449a1cc1ec93" {
		t.Fatalf("expectations = %#v", sink.expectations)
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
