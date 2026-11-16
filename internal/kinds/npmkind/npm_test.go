package npmkind

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/kindtest"
	"github.com/mmrzaf/larder/testutil"
)

func newKind(t *testing.T, opts config.NPMOptions) kinds.Kind {
	t.Helper()
	kind, err := New(config.Repo{
		Name: "npm", Kind: "npm", Mount: "/npm", NPM: &opts,
		Upstreams: []config.Upstream{{URL: "https://npm.mirror.example/"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

func TestMatrix_N01_Classification(t *testing.T) {
	kind := newKind(t, config.NPMOptions{Audit: "passthrough", AuditTimeout: config.Duration(3 * time.Second)})
	kindtest.Table(t, kind, []kindtest.Case{
		{Name: "packument", Path: "/demo", Want: kinds.Plan{Class: kinds.Mutable, Key: "/demo", UpstreamPath: "/demo", Variant: "full"}},
		{Name: "packument abbrev", Path: "/demo", Header: http.Header{"Accept": []string{"application/vnd.npm.install-v1+json"}}, Want: kinds.Plan{Class: kinds.Mutable, Key: "/demo", UpstreamPath: "/demo", Variant: "abbrev", UpstreamHdr: http.Header{"Accept": []string{"application/vnd.npm.install-v1+json"}}}},
		{Name: "scoped encoded", Path: "/@scope%2fdemo", Want: kinds.Plan{Class: kinds.Mutable, Key: "/@scope/demo", UpstreamPath: "/@scope%2fdemo", Variant: "full"}},
		{Name: "scoped slash", Path: "/@scope/demo", Want: kinds.Plan{Class: kinds.Mutable, Key: "/@scope/demo", UpstreamPath: "/@scope/demo", Variant: "full"}},
		{Name: "version document", Path: "/demo/1.0.0", Want: kinds.Plan{Class: kinds.Mutable, Key: "/demo/1.0.0", UpstreamPath: "/demo/1.0.0"}},
		{Name: "scoped version document", Path: "/@scope/demo/1.0.0", Want: kinds.Plan{Class: kinds.Mutable, Key: "/@scope/demo/1.0.0", UpstreamPath: "/@scope/demo/1.0.0"}},
		{Name: "tarball", Path: "/demo/-/demo-1.0.0.tgz", Want: kinds.Plan{Class: kinds.Immutable, Key: "/demo/-/demo-1.0.0.tgz", UpstreamPath: "/demo/-/demo-1.0.0.tgz", Binary: true}},
		{Name: "scoped tarball", Path: "/@scope/demo/-/demo-1.0.0.tgz", Want: kinds.Plan{Class: kinds.Immutable, Key: "/@scope/demo/-/demo-1.0.0.tgz", UpstreamPath: "/@scope/demo/-/demo-1.0.0.tgz", Binary: true}},
		{Name: "mirror tarball", Path: "/mirror/demo/-/demo-1.0.0.tgz", Want: kinds.Plan{Class: kinds.Immutable, Key: "/mirror/demo/-/demo-1.0.0.tgz", UpstreamPath: "/mirror/demo/-/demo-1.0.0.tgz", Binary: true}},
		{Name: "ping", Path: "/-/ping", Want: kinds.Plan{Class: kinds.Local, Key: "/-/ping", UpstreamPath: "/-/ping", Local: &kinds.LocalResponse{Status: http.StatusOK, Body: []byte("{}"), ContentType: "application/json"}}},
		{Name: "search", Path: "/-/v1/search", RawQuery: "text=demo", Want: kinds.Plan{Class: kinds.Passthrough, Key: "/-/v1/search", UpstreamPath: "/-/v1/search?text=demo", FailureStatus: http.StatusBadGateway}},
		{Name: "audit", Method: http.MethodPost, Path: "/-/npm/v1/security/audits", Want: kinds.Plan{Class: kinds.Passthrough, Key: "/-/npm/v1/security/audits", UpstreamPath: "/-/npm/v1/security/audits", AllowPost: true, Timeout: 3 * time.Second, FailureStatus: http.StatusServiceUnavailable, UpstreamHdr: http.Header{"Content-Type": []string{"application/json"}}}},
		{Name: "audit quick", Method: http.MethodPost, Path: "/-/npm/v1/security/audits/quick", Want: kinds.Plan{Class: kinds.Passthrough, Key: "/-/npm/v1/security/audits/quick", UpstreamPath: "/-/npm/v1/security/audits/quick", AllowPost: true, Timeout: 3 * time.Second, FailureStatus: http.StatusServiceUnavailable, UpstreamHdr: http.Header{"Content-Type": []string{"application/json"}}}},
		{Name: "other slash path", Path: "/-/dist-tags", Want: kinds.Plan{Class: kinds.Mutable, Key: "/-/dist-tags", UpstreamPath: "/-/dist-tags"}},
		{Name: "root", Path: "/", Want: kinds.Plan{Class: kinds.Deny, Key: "/", UpstreamPath: "/"}},
		{Name: "extra segments", Path: "/demo/1.0.0/extra", Want: kinds.Plan{Class: kinds.Deny, Key: "/demo/1.0.0/extra", UpstreamPath: "/demo/1.0.0/extra"}},
	})

	stub := newKind(t, config.NPMOptions{Audit: "stub"})
	plan, err := stub.Classify(kinds.Request{Method: http.MethodPost, Path: "/-/npm/v1/security/audits"})
	if err != nil || plan.Class != kinds.Local || string(plan.Local.Body) != "{}" {
		t.Fatalf("audit stub plan = %#v, %v", plan, err)
	}
	deny := newKind(t, config.NPMOptions{Audit: "deny"})
	plan, err = deny.Classify(kinds.Request{Method: http.MethodPost, Path: "/-/npm/v1/security/audits"})
	if err != nil || plan.Class != kinds.Deny {
		t.Fatalf("audit deny plan = %#v, %v", plan, err)
	}
}

func TestNpmRewriteAndIngest(t *testing.T) {
	kind := newKind(t, config.NPMOptions{Audit: "passthrough"})
	plan, err := kind.Classify(kinds.Request{Path: "/demo"})
	if err != nil {
		t.Fatal(err)
	}
	raw := testutil.Fixture(t, "npm", "demo.json")
	rewritten, err := plan.Hooks.Rewrite(raw, "https://larder.example")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rewritten, []byte(`"tarball": "https://larder.example/npm/demo/-/demo-1.0.0.tgz"`)) {
		t.Fatalf("rewritten packument = %s", rewritten)
	}
	if bytes.Contains(rewritten, []byte("registry.npmjs.org")) {
		t.Fatalf("official registry host was not replaced: %s", rewritten)
	}
	again, err := plan.Hooks.Rewrite(rewritten, "https://larder.example")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, rewritten) {
		t.Fatal("rewriting is not idempotent")
	}
	other, err := plan.Hooks.Rewrite(raw, "https://other.example")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(other, []byte("https://other.example/npm/demo/-/demo-1.0.0.tgz")) {
		t.Fatalf("public base change did not change output: %s", other)
	}

	mirrorKind, err := New(config.Repo{
		Name: "npm", Kind: "npm", Mount: "/npm", NPM: &config.NPMOptions{Audit: "passthrough"},
		Upstreams: []config.Upstream{{URL: "https://mirror.example/npm/"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mirrorPlan, err := mirrorKind.Classify(kinds.Request{Path: "/demo"})
	if err != nil {
		t.Fatal(err)
	}
	mirrorRaw := testutil.Fixture(t, "npm", "mirror.json")
	mirrorOut, err := mirrorPlan.Hooks.Rewrite(mirrorRaw, "https://larder.example")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(mirrorOut, []byte("https://larder.example/npm/demo/-/demo-1.0.0.tgz")) {
		t.Fatalf("mirror base was not rewritten: %s", mirrorOut)
	}

	sink := &recordingSink{}
	ref := kinds.ObjectRef{Repo: "npm", Key: "/demo#full", ContentType: "application/json"}
	if err := plan.Hooks.Ingest(context.Background(), ref, bytes.NewReader(raw), sink); err != nil {
		t.Fatal(err)
	}
	want := "/demo/-/demo-1.0.0.tgz"
	if digest, ok := sink.expectations[want]; !ok || digest == "" {
		t.Fatalf("tarball expectation missing: %#v", sink.expectations)
	}

	scoped, err := kind.Classify(kinds.Request{Path: "/@scope%2fdemo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := scoped.Hooks.Ingest(context.Background(), kinds.ObjectRef{Repo: "npm", Key: "/@scope/demo#full"}, bytes.NewReader(testutil.Fixture(t, "npm", "scoped.json")), sink); err != nil {
		t.Fatal(err)
	}
	if _, ok := sink.expectations["/@scope/demo/-/demo-1.0.0.tgz"]; !ok {
		t.Fatalf("scoped tarball expectation missing: %#v", sink.expectations)
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

func TestNpmParsePackumentErrors(t *testing.T) {
	for _, body := range []string{"", "[]", `{"versions":[]}`, `{"versions":{"1.0.0":`} {
		_ = parsePackument(strings.NewReader(body), func(dist) {})
	}
}

func FuzzParsePackument(f *testing.F) {
	f.Add(`{"versions":{"1.0.0":{"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-1.0.0.tgz","integrity":"sha512-AAAA"}}}}`)
	f.Add(`{"name":"demo"}`)
	f.Add(`{"versions":[]}`)
	f.Fuzz(func(t *testing.T, body string) {
		_ = parsePackument(strings.NewReader(body), func(dist) {})
	})
}

func TestPreferredDigestOrder(t *testing.T) {
	sha512 := "sha512-" + strings.Repeat("A", 86) + "=="
	sha256 := "sha256-" + strings.Repeat("B", 43) + "="
	sha1 := "sha1-" + strings.Repeat("C", 27) + "="
	for _, tt := range []struct {
		integrity string
		shasum    string
		want      string
	}{
		{sha512 + " " + sha256 + " " + sha1, strings.Repeat("d", 40), "sha512"},
		{sha256 + " " + sha1, "", "sha256"},
		{sha1, "", "sha1"},
		{"", strings.Repeat("e", 40), "sha1"},
	} {
		digest, ok := preferredDigest(tt.integrity, tt.shasum)
		if !ok || digest.Algo != tt.want {
			t.Fatalf("preferredDigest(%q, %q) = %#v, %v; want %s", tt.integrity, tt.shasum, digest, ok, tt.want)
		}
	}
	if _, ok := preferredDigest("crc-abc", "zz"); ok {
		t.Fatal("invalid digests accepted")
	}
}
