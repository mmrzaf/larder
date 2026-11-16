package aptkind

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func newKind(t *testing.T, consistency bool) kinds.Kind {
	t.Helper()
	kind, err := New(config.Repo{Name: "apt", Kind: "apt", Mount: "/apt", APT: &config.APTOptions{SuiteConsistency: consistency}})
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

func TestMatrix_A01_Classification(t *testing.T) {
	kind := newKind(t, true)
	digest := strings.Repeat("ab", 32)
	kindtest.Table(t, kind, []kindtest.Case{
		{Name: "pool", Path: "/pool/main/d/demo/demo_1.0_amd64.deb", Want: kinds.Plan{Class: kinds.Immutable, Key: "/pool/main/d/demo/demo_1.0_amd64.deb", UpstreamPath: "/pool/main/d/demo/demo_1.0_amd64.deb", Binary: true}},
		{Name: "pool directory", Path: "/pool/main/d/demo/", Want: kinds.Plan{Class: kinds.Immutable, Key: "/pool/main/d/demo/", UpstreamPath: "/pool/main/d/demo/", Binary: true}},
		{Name: "by hash", Path: "/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + digest, Want: kinds.Plan{Class: kinds.Immutable, Key: "/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + digest, UpstreamPath: "/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + digest, Binary: true}},
		{Name: "release", Path: "/dists/bookworm/Release", Want: kinds.Plan{Class: kinds.Mutable, Key: "/dists/bookworm/Release", UpstreamPath: "/dists/bookworm/Release", FreshTTL: 300 * time.Second}},
		{Name: "inrelease", Path: "/dists/bookworm/InRelease", Want: kinds.Plan{Class: kinds.Mutable, Key: "/dists/bookworm/InRelease", UpstreamPath: "/dists/bookworm/InRelease", FreshTTL: 300 * time.Second}},
		{Name: "packages", Path: "/dists/bookworm/main/binary-amd64/Packages.gz", Want: kinds.Plan{Class: kinds.Mutable, Key: "/dists/bookworm/main/binary-amd64/Packages.gz", UpstreamPath: "/dists/bookworm/main/binary-amd64/Packages.gz", FreshTTL: 300 * time.Second}},
		{Name: "bad by hash algorithm", Path: "/dists/bookworm/by-hash/CRC32/" + digest, Want: kinds.Plan{Class: kinds.Deny, Key: "/dists/bookworm/by-hash/CRC32/" + digest, UpstreamPath: "/dists/bookworm/by-hash/CRC32/" + digest}},
		{Name: "bad by hash digest", Path: "/dists/bookworm/by-hash/SHA256/nothex", Want: kinds.Plan{Class: kinds.Deny, Key: "/dists/bookworm/by-hash/SHA256/nothex", UpstreamPath: "/dists/bookworm/by-hash/SHA256/nothex"}},
		{Name: "project", Path: "/project/trace/host", Want: kinds.Plan{Class: kinds.Deny, Key: "/project/trace/host", UpstreamPath: "/project/trace/host"}},
		{Name: "indices", Path: "/indices/Uploaders", Want: kinds.Plan{Class: kinds.Deny, Key: "/indices/Uploaders", UpstreamPath: "/indices/Uploaders"}},
		{Name: "ls-lr", Path: "/ls-lR.gz", Want: kinds.Plan{Class: kinds.Deny, Key: "/ls-lR.gz", UpstreamPath: "/ls-lR.gz"}},
		{Name: "other", Path: "/README", Want: kinds.Plan{Class: kinds.Deny, Key: "/README", UpstreamPath: "/README"}},
	})

	byHash, err := kind.Classify(kinds.Request{Path: "/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + digest})
	if err != nil {
		t.Fatal(err)
	}
	digests := byHash.Hooks.Expect(byHash.Key)
	if len(digests) != 1 || digests[0].Algo != "sha256" || digests[0].Hex != digest {
		t.Fatalf("by-hash expectation = %#v", digests)
	}
}

func TestReleaseIngest(t *testing.T) {
	kind := newKind(t, true)
	for _, name := range []string{"Release", "InRelease"} {
		plan, err := kind.Classify(kinds.Request{Path: "/dists/bookworm/" + name})
		if err != nil {
			t.Fatal(err)
		}
		sink := &recordingSink{}
		if err := plan.Hooks.Ingest(context.Background(), kinds.ObjectRef{Repo: "apt", Key: plan.Key}, bytes.NewReader(testutil.Fixture(t, "apt", name)), sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.expectations) < 4 {
			t.Fatalf("%s expectations = %#v", name, sink.expectations)
		}
		if _, ok := sink.expectations["/dists/bookworm/main/binary-amd64/Packages.gz"]; !ok {
			t.Fatalf("%s is missing the Packages.gz expectation: %#v", name, sink.expectations)
		}
		if len(sink.stale) != 1 || sink.stale[0] != "/dists/bookworm/**" {
			t.Fatalf("%s stale patterns = %#v", name, sink.stale)
		}
	}
	disabled := newKind(t, false)
	plan, err := disabled.Classify(kinds.Request{Path: "/dists/bookworm/Release"})
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	if err := plan.Hooks.Ingest(context.Background(), kinds.ObjectRef{Repo: "apt", Key: plan.Key}, bytes.NewReader(testutil.Fixture(t, "apt", "Release")), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.stale) != 0 {
		t.Fatal("suite consistency disabled but objects were marked stale")
	}
}

func TestMatrix_A03_PackagesIngest(t *testing.T) {
	deb := []byte("fixture deb bytes")
	sum := sha256.Sum256(deb)
	digest := hex.EncodeToString(sum[:])
	kind := newKind(t, true)
	for _, name := range []string{"Packages", "Packages.gz", "Packages.xz", "Packages.bz2"} {
		key := "/dists/bookworm/main/binary-amd64/" + name
		plan, err := kind.Classify(kinds.Request{Path: key})
		if err != nil {
			t.Fatal(err)
		}
		body := testutil.Fixture(t, "apt", name)
		sink := &recordingSink{}
		if err := plan.Hooks.Ingest(context.Background(), kinds.ObjectRef{Repo: "apt", Key: key}, bytes.NewReader(body), sink); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		expectation, ok := sink.expectations["/pool/main/d/demo/demo_1.0_amd64.deb"]
		if !ok || expectation != "sha256:"+digest {
			t.Fatalf("%s expectations = %#v", name, sink.expectations)
		}
	}
}

func TestPackagesCompressedBomb(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	chunk := bytes.Repeat([]byte("A"), 1<<20)
	for i := 0; i < 65; i++ {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	kind := newKind(t, true)
	plan, err := kind.Classify(kinds.Request{Path: "/dists/bookworm/main/binary-amd64/Packages.gz"})
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	err = plan.Hooks.Ingest(context.Background(), kinds.ObjectRef{Repo: "apt", Key: plan.Key}, bytes.NewReader(compressed.Bytes()), sink)
	if err == nil {
		t.Fatal("compressed bomb was accepted")
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

func FuzzReleaseIngest(f *testing.F) {
	f.Add([]byte("SHA256:\n aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 1 main/binary-amd64/Packages\n"))
	f.Add([]byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\nSHA256:\n- abc 1 x\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, body []byte) {
		kind := newKind(t, true)
		plan, err := kind.Classify(kinds.Request{Path: "/dists/bookworm/Release"})
		if err != nil {
			t.Fatal(err)
		}
		_ = plan.Hooks.Ingest(context.Background(), kinds.ObjectRef{Repo: "apt", Key: plan.Key}, bytes.NewReader(body), &recordingSink{})
	})
}

func FuzzPackagesIngest(f *testing.F) {
	f.Add([]byte("Package: demo\nFilename: pool/demo.deb\nSHA256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n\n"))
	f.Add([]byte("Package: demo\nSHA256: bad\n\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, body []byte) {
		kind := newKind(t, true)
		plan, err := kind.Classify(kinds.Request{Path: "/dists/bookworm/main/binary-amd64/Packages"})
		if err != nil {
			t.Fatal(err)
		}
		_ = plan.Hooks.Ingest(context.Background(), kinds.ObjectRef{Repo: "apt", Key: plan.Key}, bytes.NewReader(body), &recordingSink{})
	})
}
