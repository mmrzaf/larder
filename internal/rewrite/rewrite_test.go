package rewrite

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

func testReplacer(t *testing.T) *Replacer {
	t.Helper()
	replacer, err := New([]Replacement{
		{Prefix: "https://registry.npmjs.org/", Base: "https://larder.example/npm/"},
		{Prefix: "https://mirror.example/npm/", Base: "https://larder.example/npm/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return replacer
}

func TestBufferGoldenAndIdempotence(t *testing.T) {
	input, err := os.ReadFile("testdata/input.txt")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/expected.txt")
	if err != nil {
		t.Fatal(err)
	}
	replacer := testReplacer(t)
	got := replacer.Buffer(input)
	if !bytes.Equal(got, want) {
		t.Fatalf("rewritten output:\n%s\nwant:\n%s", got, want)
	}
	if again := replacer.Buffer(got); !bytes.Equal(again, got) {
		t.Fatalf("rewrite is not idempotent:\n%s", again)
	}
}

func TestStreamSplitsPrefixes(t *testing.T) {
	replacer := testReplacer(t)
	input := "xhttps://registry.npmjs.org/demo/-/demo.tgz yhttps://mirror.example/npm/demo.tgz"
	var out bytes.Buffer
	if err := replacer.Stream(&out, iotest.OneByteReader(strings.NewReader(input))); err != nil {
		t.Fatal(err)
	}
	want := "xhttps://larder.example/npm/demo/-/demo.tgz yhttps://larder.example/npm/demo.tgz"
	if out.String() != want {
		t.Fatalf("stream output = %q, want %q", out.String(), want)
	}
	if _, err := New([]Replacement{{Prefix: ""}}); err == nil {
		t.Fatal("empty prefix accepted")
	}
	if err := (*Replacer)(nil).Stream(&out, strings.NewReader("raw")); err != nil || !strings.HasSuffix(out.String(), "raw") {
		t.Fatalf("nil replacer stream = %v, %q", err, out.String())
	}
}

func TestJSONField(t *testing.T) {
	replacer := testReplacer(t)
	input := []byte(`{"other":"https://registry.npmjs.org/keep","dist":{"tarball": "https://registry.npmjs.org/demo/-/demo.tgz","integrity":"sha512-x"}}`)
	out, err := replacer.JSONField(input, "tarball")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"other":"https://registry.npmjs.org/keep"`)) {
		t.Fatalf("non-tarball field was rewritten: %s", out)
	}
	if !bytes.Contains(out, []byte(`"tarball": "https://larder.example/npm/demo/-/demo.tgz"`)) {
		t.Fatalf("tarball field was not rewritten: %s", out)
	}
	if again, err := replacer.JSONField(out, "tarball"); err != nil || !bytes.Equal(again, out) {
		t.Fatalf("JSON rewrite is not idempotent: %s, %v", again, err)
	}
	if _, err := replacer.JSONField(input, ""); err == nil {
		t.Fatal("empty field accepted")
	}
	prefixes := replacer.Prefixes()
	if len(prefixes) != 2 || prefixes[0] != "https://registry.npmjs.org/" {
		t.Fatalf("prefixes = %#v", prefixes)
	}
	if base, ok := replacer.Base("https://mirror.example/npm/"); !ok || base != "https://larder.example/npm/" {
		t.Fatalf("base = %q, %v", base, ok)
	}
	if rest, ok := replacer.TrimPrefix("https://mirror.example/npm/demo"); !ok || rest != "demo" {
		t.Fatalf("trim = %q, %v", rest, ok)
	}
}
