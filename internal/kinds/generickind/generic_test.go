package generickind

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/kindtest"
)

func genericRepo(t *testing.T, opts config.GenericOptions) config.Repo {
	t.Helper()
	return config.Repo{Name: "generic", Kind: "generic", Generic: &opts}
}

func TestGenericClassification(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	opts := config.GenericOptions{
		DefaultClass: "deny",
		KeepQuery:    false,
		RejectHTML:   true,
		Rules: []config.GenericRule{
			{Match: `^/a`, Class: "immutable"},
			{Match: `^/a/b`, Class: "mutable"},
			{Match: `^/m`, Class: "mutable", FreshTTL: durationPtr(600 * time.Second)},
			{Match: `^/d`, Class: "deny"},
		},
		Expect: []config.GenericExpect{{Path: "/a/file", SHA256: sha}},
		Companion: []config.GenericCompanion{
			{Suffix: ".sha256", Algo: "sha256"},
		},
	}
	kind, err := New(genericRepo(t, opts))
	if err != nil {
		t.Fatal(err)
	}

	kindtest.Table(t, kind, []kindtest.Case{
		{Name: "default deny", Path: "/unmatched", Want: kinds.Plan{Class: kinds.Deny, Key: "/unmatched", UpstreamPath: "/unmatched"}},
		{Name: "first rule wins", Path: "/a/b/file.zip", Want: kinds.Plan{Class: kinds.Immutable, Key: "/a/b/file.zip", UpstreamPath: "/a/b/file.zip", Binary: true}},
		{Name: "mutable with ttl", Path: "/m", Want: kinds.Plan{Class: kinds.Mutable, Key: "/m", UpstreamPath: "/m", FreshTTL: 600 * time.Second}},
		{Name: "deny rule", Path: "/d/x", Want: kinds.Plan{Class: kinds.Deny, Key: "/d/x", UpstreamPath: "/d/x"}},
		{Name: "html not binary", Path: "/a/page.html", Want: kinds.Plan{Class: kinds.Immutable, Key: "/a/page.html", UpstreamPath: "/a/page.html"}},
		{Name: "query ignored in key", Path: "/x", RawQuery: "a=1", Want: kinds.Plan{Class: kinds.Deny, Key: "/x", UpstreamPath: "/x?a=1"}},
	})

	opts.KeepQuery = true
	queryKind, err := New(genericRepo(t, opts))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := queryKind.Classify(kinds.Request{Path: "/x", RawQuery: "a=1"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Key != "/x?a=1" || plan.UpstreamPath != "/x?a=1" {
		t.Fatalf("keep_query plan = %#v", plan)
	}

	immutable, err := kind.Classify(kinds.Request{Path: "/a/file"})
	if err != nil {
		t.Fatal(err)
	}
	if immutable.Hooks.Expect == nil {
		t.Fatal("expect hook is missing")
	}
	digests := immutable.Hooks.Expect("/a/file")
	if len(digests) != 1 || digests[0] != (integrity.Digest{Algo: "sha256", Hex: sha}) {
		t.Fatalf("expect digests = %#v", digests)
	}
	if got := immutable.Hooks.Companions("/a/file"); len(got) != 1 || got[0].Suffix != ".sha256" {
		t.Fatalf("companions = %#v", got)
	}
}

func TestGenericRejectHTMLDisabled(t *testing.T) {
	kind, err := New(genericRepo(t, config.GenericOptions{
		DefaultClass: "mutable",
		RejectHTML:   false,
		Rules:        []config.GenericRule{{Match: `^/`, Class: "immutable"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := kind.Classify(kinds.Request{Path: "/x.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Binary {
		t.Fatal("binary set with reject_html disabled")
	}
}

func TestGenericConstructorErrors(t *testing.T) {
	if _, err := New(config.Repo{Name: "x", Kind: "generic"}); err == nil {
		t.Fatal("missing options accepted")
	}
	for _, opts := range []config.GenericOptions{
		{Rules: []config.GenericRule{{Match: `[`, Class: "immutable"}}},
		{Rules: []config.GenericRule{{Match: `^/`, Class: "wrong"}}},
		{Expect: []config.GenericExpect{{Path: "/x", SHA256: "bad"}}},
		{Companion: []config.GenericCompanion{{Suffix: ".sha256"}}},
	} {
		if _, err := New(genericRepo(t, opts)); err == nil {
			t.Errorf("invalid options accepted: %#v", opts)
		}
	}
}

func TestRegistry(t *testing.T) {
	registry := kinds.NewRegistry()
	if err := registry.Register("generic", New); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("generic", New); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if err := registry.Register("", New); err == nil {
		t.Fatal("empty registration accepted")
	}
	repo := genericRepo(t, config.GenericOptions{DefaultClass: "deny"})
	kind, err := registry.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	if kind.Name() != "generic" {
		t.Fatalf("kind name = %q", kind.Name())
	}
	if _, err := registry.New(config.Repo{Name: "x", Kind: "missing"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	var status *kinds.StatusError
	if err := kinds.Status(http.StatusForbidden, "no"); !errors.As(err, &status) || status.Code != http.StatusForbidden {
		t.Fatalf("status error = %v", err)
	}
}

func durationPtr(d time.Duration) *config.Duration {
	value := config.Duration(d)
	return &value
}
