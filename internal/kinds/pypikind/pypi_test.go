package pypikind

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/kindtest"
	"github.com/mmrzaf/larder/testutil"
)

func newKind(t *testing.T) kinds.Kind {
	t.Helper()
	kind, err := New(config.Repo{
		Name: "pypi", Kind: "pypi", Mount: "/pypi",
		Upstreams: []config.Upstream{{URL: "https://pypi.org"}},
		PyPI:      &config.PyPIOptions{FilesHosts: []string{"files.pythonhosted.org"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

func TestMatrix_P01_Classification(t *testing.T) {
	kind := newKind(t)
	kindtest.Table(t, kind, []kindtest.Case{
		{Name: "root simple", Path: "/simple/", Want: kinds.Plan{Class: kinds.Mutable, Key: "/simple/", UpstreamPath: "/simple/", FreshTTL: 3600e9, Variant: "html"}},
		{Name: "root simple json", Path: "/simple/", Header: http.Header{"Accept": []string{"application/vnd.pypi.simple.v1+json"}}, Want: kinds.Plan{Class: kinds.Mutable, Key: "/simple/", UpstreamPath: "/simple/", FreshTTL: 3600e9, Variant: "json", UpstreamHdr: http.Header{"Accept": []string{"application/vnd.pypi.simple.v1+json"}}}},
		{Name: "project", Path: "/simple/demo/", Want: kinds.Plan{Class: kinds.Mutable, Key: "/simple/demo/", UpstreamPath: "/simple/demo/", Variant: "html"}},
		{Name: "normalized redirect", Path: "/simple/Demo_Name/", Want: kinds.Plan{Class: 0, Key: "/simple/Demo_Name/", UpstreamPath: "/simple/Demo_Name/", Redirect: "/pypi/simple/demo-name/"}},
		{Name: "missing slash", Path: "/simple/demo", Want: kinds.Plan{Class: 0, Key: "/simple/demo", UpstreamPath: "/simple/demo", Redirect: "/pypi/simple/demo/"}},
		{Name: "simple root redirect", Path: "/simple", Want: kinds.Plan{Class: 0, Key: "/simple", UpstreamPath: "/simple", Redirect: "/pypi/simple/"}},
		{Name: "json api", Path: "/demo/json", Want: kinds.Plan{Class: kinds.Mutable, Key: "/demo/json", UpstreamPath: "/demo/json"}},
		{Name: "json api version", Path: "/demo/1.0.0/json", Want: kinds.Plan{Class: kinds.Mutable, Key: "/demo/1.0.0/json", UpstreamPath: "/demo/1.0.0/json"}},
		{Name: "files", Path: "/_files/files.pythonhosted.org/packages/demo.whl", Want: kinds.Plan{Class: kinds.Immutable, Key: "/_files/files.pythonhosted.org/packages/demo.whl", UpstreamPath: "/_files/files.pythonhosted.org/packages/demo.whl", Binary: true}},
		{Name: "relative wheel", Path: "/packages/demo.whl", Want: kinds.Plan{Class: kinds.Immutable, Key: "/packages/demo.whl", UpstreamPath: "/packages/demo.whl", Binary: true}},
		{Name: "relative sdist", Path: "/packages/demo.tar.gz", Want: kinds.Plan{Class: kinds.Immutable, Key: "/packages/demo.tar.gz", UpstreamPath: "/packages/demo.tar.gz", Binary: true}},
		{Name: "metadata", Path: "/packages/demo.whl.metadata", Want: kinds.Plan{Class: kinds.Immutable, Key: "/packages/demo.whl.metadata", UpstreamPath: "/packages/demo.whl.metadata", Binary: true}},
		{Name: "denied host", Path: "/_files/evil.example/packages/demo.whl", WantErr: http.StatusForbidden},
		{Name: "denied path", Path: "/demo/other", Want: kinds.Plan{Class: kinds.Deny, Key: "/demo/other", UpstreamPath: "/demo/other"}},
		{Name: "root", Path: "/", Want: kinds.Plan{Class: kinds.Deny, Key: "/", UpstreamPath: "/"}},
	})
}

func TestPyPIRewriteAndIngest(t *testing.T) {
	kind := newKind(t)
	plan, err := kind.Classify(kinds.Request{Path: "/simple/demo/"})
	if err != nil {
		t.Fatal(err)
	}
	htmlRaw := testutil.Fixture(t, "pypi", "demo.html")
	rewritten, err := plan.Hooks.Rewrite(htmlRaw, "https://larder.example")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rewritten, []byte("https://larder.example/pypi/_files/files.pythonhosted.org/packages/demo-1.0.0-py3-none-any.whl#sha256=")) {
		t.Fatalf("absolute file URL was not rewritten: %s", rewritten)
	}
	if !bytes.Contains(rewritten, []byte(`href="../../packages/demo-1.0.0.tar.gz#sha256=`)) {
		t.Fatalf("relative link was changed: %s", rewritten)
	}
	if bytes.Contains(rewritten, []byte("files.pythonhosted.org/packages")) && bytes.Count(rewritten, []byte("_files/files.pythonhosted.org")) != 1 {
		t.Fatalf("unexpected replacements: %s", rewritten)
	}
	again, err := plan.Hooks.Rewrite(rewritten, "https://larder.example")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, rewritten) {
		t.Fatal("rewriting is not idempotent")
	}

	jsonPlan, err := kind.Classify(kinds.Request{Path: "/simple/demo/", Header: http.Header{"Accept": []string{"application/vnd.pypi.simple.v1+json"}}})
	if err != nil {
		t.Fatal(err)
	}
	jsonRaw := testutil.Fixture(t, "pypi", "demo.json")
	jsonOut, err := jsonPlan.Hooks.Rewrite(jsonRaw, "https://larder.example")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(jsonOut, []byte(`"url": "https://larder.example/pypi/_files/files.pythonhosted.org/packages/demo-1.0.0-py3-none-any.whl#sha256=`)) {
		t.Fatalf("JSON url was not rewritten: %s", jsonOut)
	}

	sink := &recordingSink{}
	ref := kinds.ObjectRef{Repo: "pypi", Key: "/simple/demo/#html", ContentType: "text/html"}
	if err := plan.Hooks.Ingest(context.Background(), ref, bytes.NewReader(htmlRaw), sink); err != nil {
		t.Fatal(err)
	}
	absoluteKey := "/_files/files.pythonhosted.org/packages/demo-1.0.0-py3-none-any.whl"
	relativeKey := "/packages/demo-1.0.0.tar.gz"
	if _, ok := sink.expectations[absoluteKey]; !ok {
		t.Fatalf("absolute expectation missing: %#v", sink.expectations)
	}
	if _, ok := sink.expectations[relativeKey]; !ok {
		t.Fatalf("relative expectation missing: %#v", sink.expectations)
	}
	if _, ok := sink.expectations[absoluteKey+".metadata"]; !ok {
		t.Fatalf("metadata expectation missing: %#v", sink.expectations)
	}

	sink = &recordingSink{}
	jsonRef := kinds.ObjectRef{Repo: "pypi", Key: "/simple/demo/#json", ContentType: "application/vnd.pypi.simple.v1+json"}
	if err := jsonPlan.Hooks.Ingest(context.Background(), jsonRef, bytes.NewReader(jsonRaw), sink); err != nil {
		t.Fatal(err)
	}
	if _, ok := sink.expectations[absoluteKey]; !ok {
		t.Fatalf("JSON absolute expectation missing: %#v", sink.expectations)
	}
	if _, ok := sink.expectations[relativeKey]; !ok {
		t.Fatalf("JSON relative expectation missing: %#v", sink.expectations)
	}
	if _, ok := sink.expectations[absoluteKey+".metadata"]; !ok {
		t.Fatalf("JSON metadata expectation missing: %#v", sink.expectations)
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

func TestPyPIParseErrors(t *testing.T) {
	kind := newKind(t).(*Kind)
	for _, body := range []string{"", "<html>", "[]", `{"files":`} {
		_, _ = parseHTMLIndex(kind, "/simple/demo/", bytes.NewBufferString(body))
		_, _ = parseJSONIndex(kind, "/simple/demo/", bytes.NewBufferString(body))
	}
}

func FuzzParseHTMLIndex(f *testing.F) {
	f.Add(`<html><body><a href="https://files.pythonhosted.org/packages/demo.whl#sha256=` + strings.Repeat("a", 64) + `">demo</a></body></html>`)
	f.Add(`<a href="../../packages/demo.tar.gz">demo</a>`)
	f.Add(`<a href="` + strings.Repeat("x", 100) + `">`)
	f.Fuzz(func(t *testing.T, body string) {
		kind := newKind(t).(*Kind)
		_, _ = parseHTMLIndex(kind, "/simple/demo/", strings.NewReader(body))
	})
}

func FuzzParseJSONIndex(f *testing.F) {
	f.Add(`{"files":[{"url":"https://files.pythonhosted.org/packages/demo.whl","hashes":{"sha256":"` + strings.Repeat("a", 64) + `"}}]}`)
	f.Add(`{"files":[]}`)
	f.Add(`{"files":[{"url":"../../packages/demo.tar.gz"}]}`)
	f.Fuzz(func(t *testing.T, body string) {
		kind := newKind(t).(*Kind)
		_, _ = parseJSONIndex(kind, "/simple/demo/", strings.NewReader(body))
	})
}
