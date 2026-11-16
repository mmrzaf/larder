package transfer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/generickind"
	"github.com/mmrzaf/larder/internal/kinds/gokind"
	"github.com/mmrzaf/larder/internal/kinds/mavenkind"
	"github.com/mmrzaf/larder/internal/store"
)

type transferKind struct{}

func (transferKind) Name() string { return "test" }
func (transferKind) Classify(req kinds.Request) (kinds.Plan, error) {
	plan := kinds.Plan{Class: kinds.Immutable, Key: req.Path, UpstreamPath: req.Path}
	if req.Path == "/rewritten" {
		plan.Class = kinds.Mutable
		plan.Hooks.Rewrite = func(raw []byte, base string) ([]byte, error) {
			return []byte(base + "|" + string(raw)), nil
		}
	}
	return plan, nil
}

type transferFixture struct {
	cfg   *config.Config
	store *store.Store
	index *index.DB
	dir   string
	repos []config.Repo
}

func newTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.PublicBaseURL = "https://larder.example"
	cfg.Repos = []config.Repo{{Name: "test", Kind: "test", Mount: "/test"}}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	fixture := &transferFixture{cfg: cfg, store: st, index: idx, dir: dir, repos: cfg.Repos}
	t.Cleanup(func() {
		_ = idx.Close()
		_ = st.Close()
	})
	return fixture
}

func (f *transferFixture) add(t *testing.T, key, class string, data []byte, contentType string) index.Object {
	t.Helper()
	writer, err := f.store.NewWriter(store.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	blob, _, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.index.PutBlob(context.Background(), index.Blob{
		SHA256: blob.SHA256, RawSize: blob.RawSize, StoredSize: blob.StoredSize,
		StoredEnc: string(blob.StoredEnc), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	object, err := f.index.PutObject(context.Background(), index.PutObjectParams{
		Repo: "test", Key: key, Class: class, SHA256: blob.SHA256,
		ContentType: contentType, Upstream: "fixture", Verified: true,
		FetchedAt: time.Unix(1_700_000_000, 0), ValidatedAt: time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func testRegistry(t *testing.T) *kinds.Registry {
	t.Helper()
	registry := kinds.NewRegistry()
	if err := registry.Register("test", func(config.Repo) (kinds.Kind, error) { return transferKind{}, nil }); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestMatrix_L13_ExportTreeServeAndRewrite(t *testing.T) {
	f := newTransferFixture(t)
	f.add(t, "/files/a", "immutable", []byte("artifact bytes"), "application/octet-stream")
	f.add(t, "/rewritten", "mutable", []byte(`{"url":"http://old"}`), "application/json")
	destination := filepath.Join(t.TempDir(), "export")
	result, err := Export(context.Background(), f.store, f.index, f.cfg, testRegistry(t), ExportOptions{
		To: destination, Format: "tree", BaseURL: "https://public.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Objects != 2 || result.Bytes == 0 {
		t.Fatalf("result = %+v", result)
	}
	manifestData, err := os.ReadFile(filepath.Join(destination, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.DataVersion != DataVersion || len(manifest.Objects) != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}
	foundRewrite := false
	for _, entry := range manifest.Objects {
		if entry.Key == "/rewritten" {
			foundRewrite = true
			data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(entry.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(data, []byte("https://public.example|")) {
				t.Fatalf("rewritten document = %q", data)
			}
			sum := sha256.Sum256(data)
			if entry.SHA256 != hex.EncodeToString(sum[:]) || entry.Size != int64(len(data)) {
				t.Fatalf("manifest digest does not describe exported bytes: %+v", entry)
			}
		}
	}
	if !foundRewrite {
		t.Fatal("rewritten object missing from manifest")
	}
	server := httptest.NewServer(http.FileServer(http.Dir(destination)))
	defer server.Close()
	response, err := http.Get(server.URL + "/test/files/a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "artifact bytes" {
		t.Fatalf("served artifact status %d body %q", response.StatusCode, body)
	}
	response, err = http.Get(server.URL + "/test/rewritten")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.HasPrefix(string(body), "https://public.example|") {
		t.Fatalf("served rewrite = %q", body)
	}
}

func TestMatrix_L14_ExportImportRoundTrip(t *testing.T) {
	f := newTransferFixture(t)
	f.add(t, "/files/a", "immutable", []byte("artifact bytes"), "application/octet-stream")
	f.add(t, "/rewritten", "mutable", []byte(`{"url":"old"}`), "application/json")
	destination := filepath.Join(t.TempDir(), "export")
	if _, err := Export(context.Background(), f.store, f.index, f.cfg, testRegistry(t), ExportOptions{
		To: destination, Format: "tree", BaseURL: "https://public.example",
	}); err != nil {
		t.Fatal(err)
	}
	targetDir := t.TempDir()
	targetStore, err := store.Open(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	targetIndex, err := index.Open(filepath.Join(targetDir, "index.db"))
	if err != nil {
		_ = targetStore.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = targetIndex.Close()
		_ = targetStore.Close()
	}()
	result, err := Import(context.Background(), targetStore, targetIndex, ImportOptions{
		From: destination, Verify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Objects != 2 || result.Blobs != 2 || result.Failed != 0 || result.Verified != 2 {
		t.Fatalf("import = %+v", result)
	}
	current, err := targetIndex.CurrentObject(context.Background(), "test", "/rewritten")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := targetStore.Open(current.SHA256, store.EncodingIdentity)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(reader)
	_ = reader.Close()
	if string(data) != `https://public.example|{"url":"old"}` {
		t.Fatalf("imported rewrite = %q", data)
	}
	again, err := Import(context.Background(), targetStore, targetIndex, ImportOptions{From: destination})
	if err != nil {
		t.Fatal(err)
	}
	if again.Skipped != 2 || again.Objects != 0 {
		t.Fatalf("repeat import = %+v", again)
	}
}

func TestTarRoundTripAndTraversal(t *testing.T) {
	f := newTransferFixture(t)
	f.add(t, "/files/a", "immutable", []byte("tar bytes"), "application/octet-stream")
	archive := filepath.Join(t.TempDir(), "export.tar")
	if _, err := Export(context.Background(), f.store, f.index, f.cfg, testRegistry(t), ExportOptions{
		To: archive, Format: "tar", BaseURL: "https://public.example",
	}); err != nil {
		t.Fatal(err)
	}
	targetDir := t.TempDir()
	targetStore, err := store.Open(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	targetIndex, err := index.Open(filepath.Join(targetDir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Import(context.Background(), targetStore, targetIndex, ImportOptions{From: archive, Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Objects != 1 || result.Verified != 1 {
		t.Fatalf("tar import = %+v", result)
	}
	_ = targetIndex.Close()
	_ = targetStore.Close()

	for _, path := range []string{"../evil", "/absolute", `C:\evil`, "dir\\evil", "a/../b"} {
		bad := filepath.Join(t.TempDir(), "bad.tar")
		writeBadTar(t, bad, path)
		dir := t.TempDir()
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		idx, err := index.Open(filepath.Join(dir, "index.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Import(context.Background(), st, idx, ImportOptions{From: bad}); err == nil {
			t.Fatalf("unsafe path %q accepted", path)
		}
		_ = idx.Close()
		_ = st.Close()
	}
}

func writeBadTar(t *testing.T, path, entryPath string) {
	t.Helper()
	entry := ManifestEntry{
		Repo: "repo", Key: "/key", Path: entryPath, Class: "immutable",
		SHA256: strings.Repeat("a", 64), Size: 1,
	}
	manifest := Manifest{DataVersion: DataVersion, Objects: []ManifestEntry{entry}}
	data, _ := json.Marshal(manifest)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	if err := writer.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o640, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
}

func TestSelectionAndLargeStreaming(t *testing.T) {
	f := newTransferFixture(t)
	large := bytes.Repeat([]byte("large"), 1<<20)
	f.add(t, "/files/large", "immutable", large, "application/octet-stream")
	f.add(t, "/files/old", "immutable", []byte("old data"), "application/octet-stream")
	destination := filepath.Join(t.TempDir(), "filtered")
	result, err := Export(context.Background(), f.store, f.index, f.cfg, testRegistry(t), ExportOptions{
		To: destination, Format: "tree", Match: "files/l*",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Objects != 1 {
		t.Fatalf("filtered export = %+v", result)
	}
	if info, err := os.Stat(filepath.Join(destination, "test/files/large")); err != nil || info.Size() != int64(len(large)) {
		t.Fatalf("large export file = %v, %v", info, err)
	}
	if err := os.Remove(filepath.Join(destination, "test/files/large")); err != nil {
		t.Fatal(err)
	}
}

func TestMatrix_L12_IngestGenericAndMaven(t *testing.T) {
	f := newTransferFixture(t)
	generic := config.Repo{
		Name: "generic", Kind: "generic", Mount: "/generic",
		Generic: &config.GenericOptions{
			DefaultClass: "deny", RejectHTML: true,
			Rules: []config.GenericRule{{Match: `^/files/.*`, Class: "immutable"}},
		},
	}
	genericKind, err := generickind.New(generic)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "files", "a"), "generic bytes")
	writeFile(t, filepath.Join(dir, "meta", "b"), "denied")
	result, err := Ingest(context.Background(), f.store, f.index, generic, genericKind, dir, IngestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored != 1 || result.Denied != 1 || result.Failed != 0 {
		t.Fatalf("generic ingest = %+v", result)
	}
	object, err := f.index.CurrentObject(context.Background(), "generic", "/files/a")
	if err != nil || object.Verified {
		t.Fatalf("generic object = %+v, %v", object, err)
	}

	maven := config.Repo{Name: "maven", Kind: "maven", Mount: "/maven", Maven: &config.MavenOptions{CompanionFetch: true}}
	mavenKind, err := mavenkind.New(maven)
	if err != nil {
		t.Fatal(err)
	}
	mavenDir := t.TempDir()
	artifact := []byte("jar bytes")
	sum := sha256.Sum256(artifact)
	writeFile(t, filepath.Join(mavenDir, "com/example/demo/1.0/demo-1.0.jar"), string(artifact))
	writeFile(t, filepath.Join(mavenDir, "com/example/demo/1.0/demo-1.0.jar.sha256"), hex.EncodeToString(sum[:])+"  demo-1.0.jar\n")
	writeFile(t, filepath.Join(mavenDir, "com/example/bad/1.0/bad-1.0.jar"), "bad bytes")
	writeFile(t, filepath.Join(mavenDir, "com/example/bad/1.0/bad-1.0.jar.sha256"), strings.Repeat("0", 64))
	result, err = Ingest(context.Background(), f.store, f.index, maven, mavenKind, mavenDir, IngestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored != 3 || result.Failed != 1 {
		t.Fatalf("maven ingest = %+v", result)
	}
	object, err = f.index.CurrentObject(context.Background(), "maven", "/com/example/demo/1.0/demo-1.0.jar")
	if err != nil || !object.Verified {
		t.Fatalf("maven object = %+v, %v", object, err)
	}
	dry, err := Ingest(context.Background(), f.store, f.index, generic, genericKind, dir, IngestOptions{DryRun: true})
	if err != nil || dry.Stored != 1 {
		t.Fatalf("dry ingest = %+v, %v", dry, err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestMatrix_L12_IngestGoCache(t *testing.T) {
	f := newTransferFixture(t)
	repo := config.Repo{Name: "go", Kind: "go", Mount: "/go", Go: &config.GoOptions{Verify: "basic"}}
	kind, err := gokind.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "example.com", "mod", "@v")
	writeFile(t, filepath.Join(base, "v1.2.3.info"), `{"Version":"v1.2.3"}`)
	writeFile(t, filepath.Join(base, "v1.2.3.mod"), "module example.com/mod\n")
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("example.com/mod@v1.2.3/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("module example.com/mod\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(base, "v1.2.3.zip"), archive.String())
	writeFile(t, filepath.Join(base, "v1.2.3.ziphash"), "h1:ignored")
	result, err := Ingest(context.Background(), f.store, f.index, repo, kind, dir, IngestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored != 3 || result.Denied != 1 || result.Failed != 0 {
		t.Fatalf("go ingest = %+v", result)
	}
	for _, key := range []string{
		"/example.com/mod/@v/v1.2.3.info",
		"/example.com/mod/@v/v1.2.3.mod",
		"/example.com/mod/@v/v1.2.3.zip",
	} {
		if _, err := f.index.CurrentObject(context.Background(), "go", key); err != nil {
			t.Fatalf("%s missing: %v", key, err)
		}
	}
}
