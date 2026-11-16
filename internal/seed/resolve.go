package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/warm"
)

// Options controls seed resolution.
type Options struct {
	BaseURL     string
	Repos       []warm.Repo
	RepoMap     map[string]string
	Resolver    string
	Concurrency int
	DryRun      bool
	KeepWorkdir bool
	Client      *http.Client
}

// Section is one resolved seed section.
type Section struct {
	Name     string      `json:"name"`
	Resolver string      `json:"resolver,omitempty"`
	Tool     string      `json:"tool,omitempty"`
	Message  string      `json:"message,omitempty"`
	Resolved []string    `json:"resolved,omitempty"`
	Items    []warm.Item `json:"items,omitempty"`
	Failed   bool        `json:"failed"`
}

// Result is the resolved content of one seed file.
type Result struct {
	PinSet   string
	Sections []Section
	Items    []warm.Item
	Warnings []string
}

type resolver struct {
	opts   Options
	file   *File
	result *Result
	seen   map[string]bool
	client *http.Client
}

// Resolve turns seed sections into direct requests, running native tools where
// requested.
func Resolve(ctx context.Context, file *File, opts Options) (Result, error) {
	if file == nil {
		return Result{}, fmt.Errorf("seed file is required")
	}
	if opts.Resolver != "" && opts.Resolver != "native" && opts.Resolver != "lockfile" {
		return Result{}, fmt.Errorf("resolver must be native or lockfile")
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	r := &resolver{
		opts: opts, file: file, client: client,
		result: &Result{PinSet: file.PinSet}, seen: make(map[string]bool),
	}
	if file.Go != nil {
		r.resolveGo(ctx)
	}
	if file.NPM != nil {
		r.resolveNPM(ctx)
	}
	if file.PyPI != nil {
		r.resolvePyPI(ctx)
	}
	if file.Crates != nil {
		r.resolveCrates(ctx)
	}
	if file.Rustup != nil {
		r.resolveRustup(ctx)
	}
	if file.Maven != nil {
		r.resolveMaven(ctx)
	}
	if file.GradleDist != nil {
		r.resolveGradleDist()
	}
	if file.Apt != nil {
		r.resolveApt(ctx)
	}
	for _, entry := range file.Generic {
		r.resolveGeneric(entry)
	}
	return *r.result, nil
}

func (r *resolver) section(name string) *Section {
	r.result.Sections = append(r.result.Sections, Section{Name: name})
	return &r.result.Sections[len(r.result.Sections)-1]
}

func (r *resolver) repoFor(kind, name string) (warm.Repo, bool) {
	if name != "" {
		for _, repo := range r.opts.Repos {
			if repo.Name == name {
				return repo, true
			}
		}
		return warm.Repo{}, false
	}
	if mapped := r.opts.RepoMap[kind]; mapped != "" {
		for _, repo := range r.opts.Repos {
			if repo.Name == mapped {
				return repo, true
			}
		}
	}
	for _, repo := range r.opts.Repos {
		if repo.Kind == kind {
			return repo, true
		}
	}
	return warm.Repo{}, false
}

func (r *resolver) add(section *Section, kind, repoName, path string, metadata bool, size int64, source string) {
	repo, ok := r.repoFor(kind, repoName)
	if !ok {
		section.Message = appendMessage(section.Message, "no "+kind+" repository configured")
		return
	}
	path = "/" + strings.TrimPrefix(path, "/")
	key := repo.Name + "\x00" + path
	if r.seen[key] {
		return
	}
	r.seen[key] = true
	item := warm.Item{Kind: repo.Kind, Repo: repo.Name, Path: path, Metadata: metadata, Size: size, Source: source}
	section.Items = append(section.Items, item)
	r.result.Items = append(r.result.Items, item)
}

func appendMessage(current, message string) string {
	if current == "" {
		return message
	}
	return current + "; " + message
}

func (r *resolver) choose(section *Section, kind, tool string, lockfile bool) string {
	if r.opts.Resolver != "" {
		return r.opts.Resolver
	}
	if tool != "" {
		if _, err := exec.LookPath(tool); err == nil {
			return "native"
		}
	}
	if lockfile {
		return "lockfile"
	}
	if tool != "" {
		section.Message = appendMessage(section.Message, tool+" is not installed")
	}
	return "lockfile"
}

func (r *resolver) missingTool(section *Section, tool string) {
	section.Failed = true
	section.Message = appendMessage(section.Message, tool+" is not installed; section skipped")
}

func (r *resolver) resolveGo(ctx context.Context) {
	section := r.section("go")
	section.Resolver = r.choose(section, "go", "go", true)
	if section.Resolver != "native" {
		for _, module := range r.file.Go.Modules {
			path, version, ok := splitAtVersion(module)
			if !ok {
				section.Message = appendMessage(section.Message, "invalid module "+module)
				continue
			}
			base := "/" + path + "/@v/" + version
			r.add(section, "go", "", base+".info", true, 0, "seed")
			r.add(section, "go", "", base+".mod", true, 0, "seed")
			r.add(section, "go", "", base+".zip", false, 0, "seed")
		}
	}
	for _, toolchain := range r.file.Go.Toolchains {
		platforms := r.file.Go.Platforms
		if len(platforms) == 0 {
			section.Message = appendMessage(section.Message, "toolchain "+toolchain+" has no platforms")
			continue
		}
		for _, platform := range platforms {
			version := "v0.0.1-go" + toolchain + "." + platform
			base := "/golang.org/toolchain/@v/" + version
			r.add(section, "go", "", base+".info", true, 0, "seed")
			r.add(section, "go", "", base+".mod", true, 0, "seed")
			r.add(section, "go", "", base+".zip", false, 0, "seed")
		}
	}
	if section.Resolver == "native" {
		r.nativeGo(ctx, section)
	}
}

func (r *resolver) nativeGo(ctx context.Context, section *Section) {
	repo, ok := r.repoFor("go", "")
	if !ok {
		section.Failed = true
		section.Message = appendMessage(section.Message, "no go repository configured")
		return
	}
	if len(r.file.Go.Modules) == 0 {
		return
	}
	if r.opts.DryRun {
		return
	}
	dir, cleanup, err := r.workdir("go")
	if err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	defer cleanup()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module seed\n\ngo 1.23\n"), 0o640); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	env := append(os.Environ(),
		"GOPROXY="+r.repoURL(repo),
		"GOSUMDB=off",
		"GONOSUMDB=*",
		"GOFLAGS=-mod=mod",
	)
	args := append([]string{"get"}, r.file.Go.Modules...)
	if output, err := runTool(ctx, dir, "go", args, env); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, "go get: "+strings.TrimSpace(output)+" "+err.Error())
		return
	}
	if output, err := runTool(ctx, dir, "go", []string{"mod", "download"}, env); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, "go mod download: "+strings.TrimSpace(output)+" "+err.Error())
	}
}

func (r *resolver) resolveNPM(ctx context.Context) {
	section := r.section("npm")
	section.Resolver = r.choose(section, "npm", "npm", false)
	if section.Resolver == "native" {
		r.nativeNPM(ctx, section)
		return
	}
	if len(r.file.NPM.Packages) > 0 {
		section.Failed = true
		section.Message = appendMessage(section.Message, "npm has no lockfile resolver; install npm")
	}
}

func (r *resolver) nativeNPM(ctx context.Context, section *Section) {
	if r.opts.DryRun {
		for _, spec := range r.file.NPM.Packages {
			name, version, ok := splitAtVersion(spec)
			if ok && exactVersion(version) {
				r.add(section, "npm", "", "/"+name, true, 0, "seed")
				r.add(section, "npm", "", npmTarballPath(name, version), false, 0, "seed")
			} else {
				section.Message = appendMessage(section.Message, "range "+spec+" needs the native resolver")
			}
		}
		return
	}
	repo, ok := r.repoFor("npm", "")
	if !ok {
		section.Failed = true
		section.Message = appendMessage(section.Message, "no npm repository configured")
		return
	}
	dir, cleanup, err := r.workdir("npm")
	if err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	defer cleanup()
	dependencies := make(map[string]string)
	for _, spec := range r.file.NPM.Packages {
		name, version, ok := splitAtVersion(spec)
		if !ok {
			section.Message = appendMessage(section.Message, "invalid package "+spec)
			continue
		}
		dependencies[name] = version
	}
	packageJSON := map[string]any{"name": "larder-seed", "version": "0.0.0", "private": true, "dependencies": dependencies}
	if err := writeJSON(filepath.Join(dir, "package.json"), packageJSON); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	env := append(os.Environ(),
		"npm_config_registry="+r.repoURL(repo),
		"npm_config_audit=false",
		"npm_config_fund=false",
	)
	if output, err := runTool(ctx, dir, "npm", []string{"install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund"}, env); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, "npm install: "+strings.TrimSpace(output)+" "+err.Error())
		return
	}
	r.appendLockfileItems(section, filepath.Join(dir, "package-lock.json"))
}

func (r *resolver) resolvePyPI(ctx context.Context) {
	section := r.section("pypi")
	section.Resolver = r.choose(section, "pypi", "pip", false)
	if section.Resolver == "native" {
		r.nativePyPI(ctx, section)
		return
	}
	for _, spec := range r.file.PyPI.Packages {
		name := pythonSpecName(spec)
		if name == "" {
			section.Message = appendMessage(section.Message, "invalid package "+spec)
			continue
		}
		r.add(section, "pypi", "", pythonIndexPath(name), true, 0, "seed")
	}
	if len(r.file.PyPI.Packages) > 0 {
		section.Failed = true
		section.Message = appendMessage(section.Message, "pypi lockfile resolver cannot discover artifacts; install pip")
	}
}

func (r *resolver) nativePyPI(ctx context.Context, section *Section) {
	if r.opts.DryRun {
		for _, spec := range r.file.PyPI.Packages {
			name := pythonSpecName(spec)
			if name != "" {
				r.add(section, "pypi", "", pythonIndexPath(name), true, 0, "seed")
			}
		}
		return
	}
	repo, ok := r.repoFor("pypi", "")
	if !ok {
		section.Failed = true
		section.Message = appendMessage(section.Message, "no pypi repository configured")
		return
	}
	dir, cleanup, err := r.workdir("pypi")
	if err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	defer cleanup()
	platforms := r.file.PyPI.Platforms
	if len(platforms) == 0 {
		platforms = []string{""}
	}
	pythons := r.file.PyPI.Python
	if len(pythons) == 0 {
		pythons = []string{""}
	}
	parsed, _ := url.Parse(r.repoURL(repo))
	for _, platform := range platforms {
		for _, python := range pythons {
			args := []string{"download", "--no-deps", "--dest", dir + "/files"}
			if r.file.PyPI.OnlyBinary {
				args = append(args, "--only-binary=:all:")
			}
			if platform != "" {
				args = append(args, "--platform", platform)
			}
			if python != "" {
				args = append(args, "--python-version", python)
			}
			if r.file.PyPI.OnlyBinary || platform != "" {
				args = append(args, "--implementation", "py", "--abi", "none")
			}
			args = append(args, "--index-url", r.repoURL(repo)+"/simple/")
			if parsed != nil && parsed.Host != "" {
				args = append(args, "--trusted-host", parsed.Hostname())
			}
			args = append(args, r.file.PyPI.Packages...)
			if output, err := runTool(ctx, dir, "pip", args, os.Environ()); err != nil {
				section.Failed = true
				section.Message = appendMessage(section.Message, "pip download "+platform+" "+python+": "+strings.TrimSpace(output)+" "+err.Error())
			}
		}
	}
}

func (r *resolver) resolveCrates(ctx context.Context) {
	section := r.section("crates")
	section.Resolver = r.choose(section, "crates", "cargo", false)
	if section.Resolver == "native" {
		r.nativeCrates(ctx, section)
		return
	}
	if len(r.file.Crates.Packages) > 0 {
		section.Failed = true
		section.Message = appendMessage(section.Message, "crates has no lockfile resolver; install cargo")
	}
	for _, spec := range r.file.Crates.Packages {
		name, version, ok := splitAtVersion(spec)
		if !ok || !exactVersion(version) {
			section.Message = appendMessage(section.Message, "range "+spec+" needs the native resolver")
			continue
		}
		r.add(section, "crates", "", "/index/"+crateIndexPath(name), true, 0, "seed")
		r.add(section, "crates", "", "/dl/"+name+"/"+version+"/download", false, 0, "seed")
	}
}

func (r *resolver) nativeCrates(ctx context.Context, section *Section) {
	if r.opts.DryRun {
		for _, spec := range r.file.Crates.Packages {
			name, version, ok := splitAtVersion(spec)
			if ok && exactVersion(version) {
				r.add(section, "crates", "", "/index/"+crateIndexPath(name), true, 0, "seed")
				r.add(section, "crates", "", "/dl/"+name+"/"+version+"/download", false, 0, "seed")
			}
		}
		return
	}
	repo, ok := r.repoFor("crates", "")
	if !ok {
		section.Failed = true
		section.Message = appendMessage(section.Message, "no crates repository configured")
		return
	}
	dir, cleanup, err := r.workdir("crates")
	if err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	defer cleanup()
	var manifest strings.Builder
	manifest.WriteString("[package]\nname = \"larder-seed\"\nversion = \"0.0.0\"\nedition = \"2021\"\n\n[dependencies]\n")
	for _, spec := range r.file.Crates.Packages {
		name, version, ok := splitAtVersion(spec)
		if !ok {
			section.Message = appendMessage(section.Message, "invalid package "+spec)
			continue
		}
		fmt.Fprintf(&manifest, "%s = %q\n", name, version)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(manifest.String()), 0o640); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o750); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "lib.rs"), nil, 0o640); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, ".cargo"), 0o750); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	config := fmt.Sprintf("[source.crates-io]\nreplace-with = \"larder\"\n\n[source.larder]\nregistry = \"sparse+%s/index/\"\n", r.repoURL(repo))
	if err := os.WriteFile(filepath.Join(dir, ".cargo", "config.toml"), []byte(config), 0o640); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	env := append(os.Environ(), "CARGO_HOME="+filepath.Join(dir, "cargo-home"))
	if output, err := runTool(ctx, dir, "cargo", []string{"fetch"}, env); err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, "cargo fetch: "+strings.TrimSpace(output)+" "+err.Error())
	}
}

func (r *resolver) resolveMaven(ctx context.Context) {
	section := r.section("maven")
	tool := "mvn"
	if _, err := exec.LookPath("gradle"); err == nil {
		tool = "gradle"
	} else if _, err := exec.LookPath("mvn"); err != nil {
		tool = ""
	}
	section.Resolver = r.choose(section, "maven", tool, true)
	section.Tool = tool
	if section.Resolver == "native" {
		if tool == "" {
			r.missingTool(section, "gradle or mvn")
			return
		}
		r.nativeMaven(ctx, section, tool)
		return
	}
	for _, coordinate := range r.file.Maven.Coordinates {
		group, artifact, version, classifier, extension, ok := parseCoordinate(coordinate)
		if !ok {
			section.Message = appendMessage(section.Message, "invalid coordinate "+coordinate)
			continue
		}
		r.mavenCoordinate(section, r.file.Maven.Repo, group, artifact, version, classifier, extension)
	}
}

func (r *resolver) nativeMaven(ctx context.Context, section *Section, tool string) {
	if r.opts.DryRun {
		for _, coordinate := range r.file.Maven.Coordinates {
			group, artifact, version, classifier, extension, ok := parseCoordinate(coordinate)
			if ok {
				r.mavenCoordinate(section, r.file.Maven.Repo, group, artifact, version, classifier, extension)
			}
		}
		return
	}
	repo, ok := r.repoFor("maven", r.file.Maven.Repo)
	if !ok {
		section.Failed = true
		section.Message = appendMessage(section.Message, "no maven repository configured")
		return
	}
	dir, cleanup, err := r.workdir("maven")
	if err != nil {
		section.Failed = true
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	defer cleanup()
	if tool == "gradle" {
		var build strings.Builder
		build.WriteString("plugins { id 'java' }\nrepositories { maven { url '")
		build.WriteString(r.repoURL(repo))
		build.WriteString("'; allowInsecureProtocol = true } }\ndependencies {\n")
		for _, coordinate := range r.file.Maven.Coordinates {
			fmt.Fprintf(&build, "  implementation '%s'\n", coordinate)
		}
		build.WriteString("}\n")
		if err := os.WriteFile(filepath.Join(dir, "build.gradle"), []byte(build.String()), 0o640); err != nil {
			section.Failed = true
			section.Message = appendMessage(section.Message, err.Error())
			return
		}
		if output, err := runTool(ctx, dir, "gradle", []string{"--no-daemon", "dependencies"}, os.Environ()); err != nil {
			section.Failed = true
			section.Message = appendMessage(section.Message, "gradle: "+strings.TrimSpace(output)+" "+err.Error())
		}
		return
	}
	for _, coordinate := range r.file.Maven.Coordinates {
		args := []string{"-q", "dependency:get", "-Dartifact=" + coordinate,
			"-DremoteRepositories=larder::default::" + r.repoURL(repo)}
		if output, err := runTool(ctx, dir, "mvn", args, os.Environ()); err != nil {
			section.Failed = true
			section.Message = appendMessage(section.Message, "mvn: "+strings.TrimSpace(output)+" "+err.Error())
		}
	}
}

func (r *resolver) mavenCoordinate(section *Section, repoName, group, artifact, version, classifier, extension string) {
	if extension == "" {
		extension = "jar"
	}
	base := "/" + strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/" + version
	r.add(section, "maven", repoName, base+"/"+artifact+"-"+version+".pom", true, 0, "seed")
	name := artifact + "-" + version
	if classifier != "" {
		name += "-" + classifier
	}
	r.add(section, "maven", repoName, base+"/"+name+"."+extension, false, 0, "seed")
}

func (r *resolver) resolveGradleDist() {
	section := r.section("gradle_dist")
	section.Resolver = "direct"
	for _, version := range r.file.GradleDist.Versions {
		for _, classifier := range []string{"bin", "all"} {
			r.add(section, "generic", r.file.GradleDist.Repo,
				"/distributions/gradle-"+version+"-"+classifier+".zip", false, 0, "seed")
		}
	}
}

func (r *resolver) resolveGeneric(entry GenericEntry) {
	section := r.section("generic")
	section.Resolver = "direct"
	for _, path := range entry.Paths {
		r.add(section, "generic", entry.Repo, "/"+strings.TrimPrefix(path, "/"), false, 0, "seed")
	}
}

func (r *resolver) appendLockfileItems(section *Section, path string) {
	plan, err := warm.Build(warm.Options{
		Paths: []string{path}, Repos: r.opts.Repos, RepoMap: r.opts.RepoMap, Only: []string{"npm"},
	})
	if err != nil {
		section.Message = appendMessage(section.Message, err.Error())
		return
	}
	for _, item := range plan.Items {
		key := item.Repo + "\x00" + item.Path
		if r.seen[key] {
			continue
		}
		r.seen[key] = true
		section.Items = append(section.Items, item)
		r.result.Items = append(r.result.Items, item)
	}
}

func (r *resolver) repoURL(repo warm.Repo) string {
	base := strings.TrimSuffix(r.opts.BaseURL, "/")
	mount := strings.Trim(repo.Mount, "/")
	if mount == "" {
		return base
	}
	return base + "/" + mount
}

func (r *resolver) workdir(name string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "larder-seed-"+name+"-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		if !r.opts.KeepWorkdir {
			_ = os.RemoveAll(dir)
		}
	}
	return dir, cleanup, nil
}

func runTool(ctx context.Context, dir, name string, args, env []string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = env
	output, err := command.CombinedOutput()
	return string(output), err
}

func splitAtVersion(spec string) (string, string, bool) {
	spec = strings.TrimSpace(spec)
	index := strings.LastIndex(spec, "@")
	if index <= 0 || index == len(spec)-1 {
		return "", "", false
	}
	return spec[:index], spec[index+1:], true
}

func exactVersion(version string) bool {
	if version == "" {
		return false
	}
	for _, r := range version {
		if r >= '0' && r <= '9' || r == '.' || r == '-' || r == '+' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			continue
		}
		return false
	}
	return version[0] >= '0' && version[0] <= '9'
}

func parseCoordinate(value string) (group, artifact, version, classifier, extension string, ok bool) {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "@"); index > 0 {
		extension = value[index+1:]
		value = value[:index]
	}
	parts := strings.Split(value, ":")
	if len(parts) < 3 {
		return "", "", "", "", "", false
	}
	group, artifact, version = parts[0], parts[1], parts[2]
	if len(parts) >= 4 {
		classifier = parts[3]
	}
	return group, artifact, version, classifier, extension, true
}

func writeJSON(path string, value any) error {
	data, err := jsonMarshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o640)
}

func jsonMarshal(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}

func npmTarballPath(name, version string) string {
	base := name
	if index := strings.LastIndex(name, "/"); index >= 0 {
		base = name[index+1:]
	}
	return "/" + name + "/-/" + base + "-" + version + ".tgz"
}

func crateIndexPath(name string) string {
	lower := strings.ToLower(name)
	switch len(lower) {
	case 1:
		return "1/" + lower
	case 2:
		return "2/" + lower
	case 3:
		return "3/" + lower[:1] + "/" + lower
	default:
		return lower[:2] + "/" + lower[2:4] + "/" + lower
	}
}

func pythonSpecName(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	if index := strings.IndexAny(spec, "<>=!~ [;"); index >= 0 {
		spec = spec[:index]
	}
	return strings.TrimSpace(spec)
}

func pythonIndexPath(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, ".", "-")
	return "/simple/" + name + "/"
}
