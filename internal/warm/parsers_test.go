package warm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRepos() []Repo {
	return []Repo{
		{Name: "go", Kind: "go", Mount: "/go"},
		{Name: "npm", Kind: "npm", Mount: "/npm"},
		{Name: "pypi", Kind: "pypi", Mount: "/pypi"},
		{Name: "crates", Kind: "crates", Mount: "/crates"},
		{Name: "maven", Kind: "maven", Mount: "/maven"},
		{Name: "generic", Kind: "generic", Mount: "/generic"},
	}
}

func writeTemp(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func itemPaths(plan Plan, repo string) []string {
	var out []string
	for _, item := range plan.Items {
		if item.Repo == repo {
			out = append(out, item.Path)
		}
	}
	return out
}

func TestMatrix_L06_LockfilePlan(t *testing.T) {
	dir := t.TempDir()
	goSum := "example.com/mod v1.2.3 h1:abc\n" +
		"example.com/mod v1.2.3/go.mod h1:def\n"
	packageLock := `{"packages":{"":{"name":"app"},"node_modules/left-pad":{"version":"1.3.0","resolved":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}}}`
	requirements := "requests==2.31.0\n" +
		"https://files.pythonhosted.org/packages/aa/bb/requests-2.31.0-py3-none-any.whl\n"
	cargoLock := `[[package]]
name = "serde"
version = "1.0.0"
source = "registry+https://github.com/rust-lang/crates.io-index"
checksum = "abc"
`
	verification := `<verification-metadata><components><component group="com.example" name="demo" version="1.0"><artifact name="demo-1.0.jar"/></component></components></verification-metadata>`
	writeTemp(t, dir, "go.sum", goSum)
	writeTemp(t, dir, "package-lock.json", packageLock)
	writeTemp(t, dir, "requirements.txt", requirements)
	writeTemp(t, dir, "Cargo.lock", cargoLock)
	writeTemp(t, dir, "gradle/verification-metadata.xml", verification)
	writeTemp(t, dir, "pom.xml", "<project/>")

	plan, err := Build(Options{Paths: []string{dir}, Repos: testRepos()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Skipped) != 1 {
		t.Fatalf("skipped = %+v", plan.Skipped)
	}
	cases := map[string][]string{
		"go":     {"/example.com/mod/@v/v1.2.3.info", "/example.com/mod/@v/v1.2.3.mod", "/example.com/mod/@v/v1.2.3.zip"},
		"npm":    {"/left-pad", "/left-pad/-/left-pad-1.3.0.tgz"},
		"pypi":   {"/simple/requests/", "/_files/files.pythonhosted.org/packages/aa/bb/requests-2.31.0-py3-none-any.whl"},
		"crates": {"/index/se/rd/serde", "/dl/serde/1.0.0/download"},
		"maven":  {"/com/example/demo/1.0/demo-1.0.pom", "/com/example/demo/1.0/demo-1.0.jar"},
	}
	for repo, want := range cases {
		got := itemPaths(plan, repo)
		if len(got) != len(want) {
			t.Fatalf("%s paths = %+v, want %+v", repo, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s paths = %+v, want %+v", repo, got, want)
			}
		}
	}
	for _, item := range plan.Items {
		if item.Path == "/left-pad" && !item.Metadata {
			t.Fatal("npm packument is not metadata")
		}
		if item.Path == "/left-pad/-/left-pad-1.3.0.tgz" && item.Metadata {
			t.Fatal("npm tarball is metadata")
		}
	}
}

func TestAdditionalLockfileParsers(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"pnpm-lock.yaml": `lockfileVersion: '9.0'
packages:
  /lodash@4.17.21: {}
  /@scope/pkg@1.2.3: {}
`,
		"yarn.lock": `# yarn lockfile v1
"lodash@^4.17.20":
  version "4.17.21"
  resolved "https://registry.yarnpkg.com/lodash/-/lodash-4.17.21.tgz"
`,
		"berry.lock": `__metadata:
  version: 6
"@scope/pkg@npm:^1.0.0":
  version: 1.0.1
`,
		"bun.lock": `{
  "lockfileVersion": 1,
  "packages": {
    "lodash": ["lodash@4.17.21", "", {}, "sha512-x"]
  }
}`,
		"poetry.lock": `[[package]]
name = "requests"
version = "2.31.0"
files = [{file = "requests-2.31.0-py3-none-any.whl", hash = "sha256:x"}]
`,
		"uv.lock": `[[package]]
name = "requests"
version = "2.31.0"
wheels = [{url = "https://files.pythonhosted.org/packages/aa/requests-2.31.0-py3-none-any.whl"}]
sdist = {url = "https://files.pythonhosted.org/packages/bb/requests-2.31.0.tar.gz"}
`,
		"Pipfile.lock": `{"default":{"requests":{"version":"==2.31.0","files":[{"file":"requests-2.31.0.whl"}]}}}`,
		"pdm.lock": `[[package]]
name = "requests"
version = "2.31.0"
files = [{url = "https://files.pythonhosted.org/packages/cc/requests-2.31.0.whl"}]
`,
		"gradle.lockfile": `com.example:demo:1.0=compileClasspath
`,
	}
	for name, body := range files {
		writeTemp(t, dir, name, body)
	}
	// Yarn berry uses the same name, so parse it separately below.
	os.Remove(filepath.Join(dir, "berry.lock"))
	writeTemp(t, dir, "berry/yarn.lock", `__metadata:
  version: 6
"@scope/pkg@npm:^1.0.0":
  version: 1.0.1
`)
	plan, err := Build(Options{Paths: []string{dir}, Repos: testRepos()})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, item := range plan.Items {
		joined += item.Repo + " " + item.Path + "\n"
	}
	for _, want := range []string{
		"/lodash", "/lodash/-/lodash-4.17.21.tgz", "/@scope/pkg", "/@scope/pkg/-/pkg-1.2.3.tgz",
		"/simple/requests/", "/_files/files.pythonhosted.org/packages/aa/requests-2.31.0-py3-none-any.whl",
	} {
		if !contains(joined, want) {
			t.Fatalf("missing %q in plan:\n%s", want, joined)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestMatrix_L07_FromCache(t *testing.T) {
	dir := t.TempDir()
	goCache := filepath.Join(dir, "go")
	writeTemp(t, goCache, "example.com/mod/@v/v1.2.3.info", "{}")
	writeTemp(t, goCache, "example.com/mod/@v/v1.2.3.mod", "module example.com/mod")
	writeTemp(t, goCache, "example.com/mod/@v/v1.2.3.zip", "zipdata")
	mavenCache := filepath.Join(dir, "maven")
	writeTemp(t, mavenCache, "com/example/demo/1.0/demo-1.0.pom", "<project/>")
	writeTemp(t, mavenCache, "com/example/demo/1.0/demo-1.0.jar", "jardata")
	writeTemp(t, mavenCache, "com/example/demo/1.0/_remote.repositories", "ignored")
	gradleCache := filepath.Join(dir, "gradle")
	writeTemp(t, gradleCache, "com.example/demo/1.0/hash/demo-1.0.jar", "gradledata")
	cargoCache := filepath.Join(dir, "cargo")
	writeTemp(t, cargoCache, "serde-1.0.0.crate", "cratedata")

	plan, err := Build(Options{
		Repos: testRepos(),
		FromCache: []string{
			"go=" + goCache, "maven=" + mavenCache,
			"gradle=" + gradleCache, "cargo=" + cargoCache,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/example.com/mod/@v/v1.2.3.info", "/example.com/mod/@v/v1.2.3.mod", "/example.com/mod/@v/v1.2.3.zip",
		"/com/example/demo/1.0/demo-1.0.pom", "/com/example/demo/1.0/demo-1.0.jar",
		"/index/se/rd/serde", "/dl/serde/1.0.0/download",
	} {
		found := false
		for _, item := range plan.Items {
			if item.Path == want {
				found = true
				if item.Size == 0 && !strings.HasPrefix(want, "/index/") {
					t.Fatalf("%s has no size", want)
				}
			}
		}
		if !found {
			t.Fatalf("missing %s in %+v", want, plan.Items)
		}
	}
	for _, item := range plan.Items {
		if item.Path == "/com/example/demo/1.0/_remote.repositories" {
			t.Fatal("metadata junk was included")
		}
	}
}

func TestURLListAndFiltering(t *testing.T) {
	dir := t.TempDir()
	list := writeTemp(t, dir, "urls.txt", "# comment\n/owner/tool/a.tar.gz\n\n/owner/tool/b.tar.gz\n")
	plan, err := Build(Options{
		Repos: testRepos(), URLList: list, URLRepo: "generic",
		Only: []string{"generic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 || plan.Items[0].Path != "/owner/tool/a.tar.gz" {
		t.Fatalf("items = %+v", plan.Items)
	}
	if _, err := Build(Options{Repos: testRepos(), URLList: list}); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(Options{Repos: testRepos(), URLList: list, URLRepo: "missing"}); err != nil {
		t.Fatal(err)
	}
	plan, err = Build(Options{Repos: testRepos(), URLList: list, URLRepo: "generic", Only: []string{"npm"}})
	if err != nil || len(plan.Items) != 0 {
		t.Fatalf("only filter = %+v, %v", plan, err)
	}
}

func FuzzLockfileParsers(f *testing.F) {
	f.Add("go.sum", []byte("example.com/mod v1.2.3 h1:x\n"))
	f.Add("package-lock.json", []byte(`{"packages":{"node_modules/a":{"version":"1.0.0"}}}`))
	f.Add("pnpm-lock.yaml", []byte("packages:\n  /a@1.0.0: {}\n"))
	f.Add("yarn.lock", []byte("\"a@^1\":\n  version \"1.0.0\"\n"))
	f.Add("bun.lock", []byte(`{"packages":{"a":["a@1.0.0"]}}`))
	f.Add("requirements.txt", []byte("a==1.0.0\n"))
	f.Add("poetry.lock", []byte("[[package]]\nname=\"a\"\nversion=\"1\"\n"))
	f.Add("Cargo.lock", []byte("[[package]]\nname=\"a\"\nversion=\"1\"\n"))
	f.Add("gradle.lockfile", []byte("a:b:1.0=x\n"))
	f.Add("verification-metadata.xml", []byte("<verification-metadata><components/></verification-metadata>"))
	f.Fuzz(func(t *testing.T, name string, body []byte) {
		switch filepath.Base(name) {
		case "go.sum", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml",
			"yarn.lock", "bun.lock", "requirements.txt", "poetry.lock", "uv.lock",
			"Pipfile.lock", "pdm.lock", "Cargo.lock", "gradle.lockfile", "verification-metadata.xml":
		default:
			return
		}
		dir := t.TempDir()
		path := filepath.Join(dir, filepath.Base(name))
		if err := os.WriteFile(path, body, 0o640); err != nil {
			t.Fatal(err)
		}
		_, _ = Build(Options{Paths: []string{path}, Repos: testRepos()})
	})
}

func TestAptURIs(t *testing.T) {
	dir := t.TempDir()
	list := writeTemp(t, dir, "apt.uris",
		"'http://deb.example/debian/pool/main/h/hello/hello_2.10-2_amd64.deb' http://deb.example 100 SHA256:abc\n"+
			"'http://larder.example/apt/debian/pool/main/g/git/git_1_amd64.deb' x 200 SHA256:def\n")
	repos := testRepos()
	repos = append(repos, Repo{Name: "apt", Kind: "apt", Mount: "/apt/debian"})
	plan, err := Build(Options{
		Paths: []string{list}, Repos: repos,
		Upstreams: []RepoUpstream{{Repo: "apt", URL: "http://deb.example/debian"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := itemPaths(plan, "apt")
	want := []string{"/pool/main/h/hello/hello_2.10-2_amd64.deb", "/pool/main/g/git/git_1_amd64.deb"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("apt paths = %+v", got)
	}
}

func TestMountedLockfileURLs(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "package-lock.json", `{"packages":{"node_modules/a":{"version":"1.0.0","resolved":"https://larder.example/npm/a/-/a-1.0.0.tgz"}}}`)
	writeTemp(t, dir, "requirements.txt", "a==1.0.0\nhttps://larder.example/pypi/_files/files.pythonhosted.org/packages/aa/a-1.0.0.whl\n")
	plan, err := Build(Options{Paths: []string{dir}, Repos: testRepos()})
	if err != nil {
		t.Fatal(err)
	}
	foundTarball, foundFile := false, false
	for _, item := range plan.Items {
		if item.Path == "/a/-/a-1.0.0.tgz" {
			foundTarball = true
		}
		if item.Path == "/_files/files.pythonhosted.org/packages/aa/a-1.0.0.whl" {
			foundFile = true
		}
	}
	if !foundTarball || !foundFile {
		t.Fatalf("mounted URLs were not normalized: %+v", plan.Items)
	}
}
