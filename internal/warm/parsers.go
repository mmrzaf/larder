package warm

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

func (b *builder) parseGoSum(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		modulePath := fields[0]
		version := fields[1]
		modOnly := strings.HasSuffix(version, "/go.mod")
		version = strings.TrimSuffix(version, "/go.mod")
		base := "/" + modulePath + "/@v/" + version
		if modOnly {
			b.add("go", base+".mod", true, 0, path)
			continue
		}
		b.add("go", base+".info", true, 0, path)
		b.add("go", base+".mod", true, 0, path)
		b.add("go", base+".zip", false, 0, path)
	}
	return scanner.Err()
}

type npmEntry struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Resolved string `json:"resolved"`
	Link     bool   `json:"link"`
}

func (b *builder) parsePackageLock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Packages     map[string]npmEntry `json:"packages"`
		Dependencies map[string]npmEntry `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse npm lockfile: %w", err)
	}
	for key, entry := range document.Packages {
		if key == "" || entry.Link {
			continue
		}
		name := entry.Name
		if name == "" {
			index := strings.LastIndex(key, "node_modules/")
			if index < 0 {
				continue
			}
			name = key[index+len("node_modules/"):]
		}
		b.npmPackage(name, entry.Version, entry.Resolved, path)
	}
	for name, entry := range document.Dependencies {
		b.npmPackage(name, entry.Version, entry.Resolved, path)
	}
	return nil
}

func (b *builder) npmPackage(name, version, resolved, source string) {
	if name == "" || version == "" {
		return
	}
	b.add("npm", "/"+name, true, 0, source)
	path := npmTarballPath(name, version)
	if resolved != "" {
		if parsed, err := url.Parse(resolved); err == nil && parsed.Path != "" {
			path = b.stripMount(parsed.EscapedPath())
		}
	}
	b.add("npm", path, false, 0, source)
}

func npmTarballPath(name, version string) string {
	base := name
	if index := strings.LastIndex(name, "/"); index >= 0 {
		base = name[index+1:]
	}
	return "/" + name + "/-/" + base + "-" + version + ".tgz"
}

func (b *builder) parsePNPMLock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Packages  map[string]any `yaml:"packages"`
		Snapshots map[string]any `yaml:"snapshots"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse pnpm lockfile: %w", err)
	}
	for name := range document.Packages {
		b.pnpmEntry(name, path)
	}
	for name := range document.Snapshots {
		b.pnpmEntry(name, path)
	}
	return nil
}

func (b *builder) pnpmEntry(key, source string) {
	key = strings.TrimPrefix(key, "/")
	if index := strings.IndexByte(key, '('); index >= 0 {
		key = key[:index]
	}
	split := strings.LastIndex(key, "@")
	if split <= 0 {
		return
	}
	name, version := key[:split], key[split+1:]
	if name == "" || version == "" || strings.ContainsAny(version, ":*") {
		return
	}
	b.add("npm", "/"+name, true, 0, source)
	b.add("npm", npmTarballPath(name, version), false, 0, source)
}

func (b *builder) parseYarnLock(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var name, version string
	flush := func() {
		if name != "" && version != "" {
			b.add("npm", "/"+name, true, 0, path)
			b.add("npm", npmTarballPath(name, version), false, 0, path)
		}
		name, version = "", ""
	}
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		if !strings.HasPrefix(line, " ") && strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "#") {
			flush()
			selector := strings.TrimSuffix(trimmed, ":")
			selector = strings.Trim(selector, `"`)
			if index := strings.IndexByte(selector, ','); index >= 0 {
				selector = selector[:index]
			}
			split := strings.LastIndex(selector, "@")
			if split > 0 {
				name = selector[:split]
			}
			continue
		}
		if strings.HasPrefix(trimmed, "version") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "version"))
			value = strings.TrimSpace(strings.TrimPrefix(value, ":"))
			version = strings.Trim(value, `"`)
		}
	}
	flush()
	return scanner.Err()
}

var bunPackagePattern = regexp.MustCompile(`"([^"]+)"\s*:\s*\[\s*"([^"]+)"`)

func (b *builder) parseBunLock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Packages map[string][]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(data, &document); err == nil && len(document.Packages) > 0 {
		for key, values := range document.Packages {
			if len(values) == 0 {
				continue
			}
			var resolved string
			if err := json.Unmarshal(values[0], &resolved); err != nil || resolved == "" {
				continue
			}
			b.bunEntry(key, resolved, path)
		}
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		match := bunPackagePattern.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		b.bunEntry(match[1], match[2], path)
	}
	return nil
}

func (b *builder) bunEntry(key, resolved, source string) {
	name, version := key, ""
	if split := strings.LastIndex(resolved, "@"); split > 0 {
		name, version = resolved[:split], resolved[split+1:]
	}
	if name == "" || version == "" || strings.Contains(version, ":") {
		return
	}
	if keyName := strings.TrimSuffix(key, "@"+version); strings.HasSuffix(key, "@"+version) {
		name = keyName
	}
	b.add("npm", "/"+name, true, 0, source)
	b.add("npm", npmTarballPath(name, version), false, 0, source)
}

func (b *builder) parseRequirements(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "-") {
			b.skip("%s: option line skipped: %s", path, line)
			continue
		}
		if index := strings.Index(line, " --hash"); index >= 0 {
			line = line[:index]
		}
		if index := strings.Index(line, " @ "); index >= 0 {
			name := normalizePythonName(pythonRequirementName(strings.TrimSpace(line[:index])))
			b.add("pypi", pythonIndexPath(name), true, 0, path)
			if filePath, ok := b.pythonFileURL(strings.TrimSpace(line[index+3:])); ok {
				b.add("pypi", filePath, false, 0, path)
			}
			continue
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			if filePath, ok := b.pythonFileURL(line); ok {
				b.add("pypi", filePath, false, 0, path)
			}
			continue
		}
		if index := strings.Index(line, "=="); index > 0 {
			name := normalizePythonName(pythonRequirementName(strings.TrimSpace(line[:index])))
			version := strings.TrimSpace(line[index+2:])
			if marker := strings.IndexByte(version, ';'); marker >= 0 {
				version = strings.TrimSpace(version[:marker])
			}
			if version == "" || strings.ContainsAny(version, "<>=!~* ") {
				b.skip("%s: unpinned requirement skipped: %s", path, line)
				continue
			}
			b.add("pypi", pythonIndexPath(name), true, 0, path)
			continue
		}
		b.skip("%s: unpinned requirement skipped: %s", path, line)
	}
	return scanner.Err()
}

type lockFile struct {
	File string `toml:"file" json:"file"`
	URL  string `toml:"url" json:"url"`
}

func (b *builder) parsePoetryLock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Package []struct {
			Name    string     `toml:"name"`
			Version string     `toml:"version"`
			Files   []lockFile `toml:"files"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse poetry lockfile: %w", err)
	}
	for _, pkg := range document.Package {
		b.add("pypi", pythonIndexPath(normalizePythonName(pkg.Name)), true, 0, path)
		for _, file := range pkg.Files {
			if file.URL != "" {
				if filePath, ok := b.pythonFileURL(file.URL); ok {
					b.add("pypi", filePath, false, 0, path)
				}
			}
		}
	}
	return nil
}

func (b *builder) parseUVLock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Package []struct {
			Name    string     `toml:"name"`
			Version string     `toml:"version"`
			Wheels  []lockFile `toml:"wheels"`
			Sdist   lockFile   `toml:"sdist"`
			Files   []lockFile `toml:"files"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse uv lockfile: %w", err)
	}
	for _, pkg := range document.Package {
		b.add("pypi", pythonIndexPath(normalizePythonName(pkg.Name)), true, 0, path)
		files := append(append([]lockFile{}, pkg.Wheels...), pkg.Files...)
		if pkg.Sdist.URL != "" {
			files = append(files, pkg.Sdist)
		}
		for _, file := range files {
			if file.URL == "" || !b.platformFile(file.URL) {
				continue
			}
			if filePath, ok := b.pythonFileURL(file.URL); ok {
				b.add("pypi", filePath, false, 0, path)
			}
		}
	}
	return nil
}

func (b *builder) platformFile(raw string) bool {
	if b.opts.Platform == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return true
	}
	name := filepath.Base(parsed.Path)
	if strings.Contains(name, b.opts.Platform) || strings.Contains(name, "none-any") ||
		strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip") {
		return true
	}
	return false
}

func (b *builder) parsePipfileLock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Default map[string]struct {
			Version string     `json:"version"`
			Files   []lockFile `json:"files"`
		} `json:"default"`
		Develop map[string]struct {
			Version string     `json:"version"`
			Files   []lockFile `json:"files"`
		} `json:"develop"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse Pipfile.lock: %w", err)
	}
	for _, section := range []map[string]struct {
		Version string     `json:"version"`
		Files   []lockFile `json:"files"`
	}{document.Default, document.Develop} {
		for name := range section {
			b.add("pypi", pythonIndexPath(normalizePythonName(name)), true, 0, path)
		}
	}
	return nil
}

func (b *builder) parsePDMLock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Package []struct {
			Name    string     `toml:"name"`
			Version string     `toml:"version"`
			Files   []lockFile `toml:"files"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse pdm lockfile: %w", err)
	}
	for _, pkg := range document.Package {
		b.add("pypi", pythonIndexPath(normalizePythonName(pkg.Name)), true, 0, path)
		for _, file := range pkg.Files {
			if file.URL != "" {
				if filePath, ok := b.pythonFileURL(file.URL); ok {
					b.add("pypi", filePath, false, 0, path)
				}
			}
		}
	}
	return nil
}

func (b *builder) parseCargoLock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Package []struct {
			Name    string `toml:"name"`
			Version string `toml:"version"`
			Source  string `toml:"source"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse Cargo.lock: %w", err)
	}
	for _, pkg := range document.Package {
		if pkg.Name == "" || pkg.Version == "" || !strings.HasPrefix(pkg.Source, "registry+") {
			continue
		}
		b.add("crates", "/index/"+crateIndexPath(pkg.Name), true, 0, path)
		b.add("crates", "/dl/"+pkg.Name+"/"+pkg.Version+"/download", false, 0, path)
	}
	return nil
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

func (b *builder) parseGradleLockfile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "empty=") {
			continue
		}
		if index := strings.IndexByte(line, '='); index >= 0 {
			line = line[:index]
		}
		parts := strings.Split(line, ":")
		if len(parts) < 3 {
			continue
		}
		b.mavenCoordinate(parts[0], parts[1], parts[2], "", "", path)
	}
	return scanner.Err()
}

type verificationMetadata struct {
	Components []struct {
		Group     string `xml:"group,attr"`
		Name      string `xml:"name,attr"`
		Version   string `xml:"version,attr"`
		Artifacts []struct {
			Name string `xml:"name,attr"`
		} `xml:"artifact"`
	} `xml:"components>component"`
}

func (b *builder) parseVerificationMetadata(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document verificationMetadata
	if err := xml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse verification metadata: %w", err)
	}
	for _, component := range document.Components {
		if component.Group == "" || component.Name == "" || component.Version == "" {
			continue
		}
		base := mavenDirectory(component.Group, component.Name, component.Version)
		b.add("maven", base+"/"+component.Name+"-"+component.Version+".pom", true, 0, path)
		if len(component.Artifacts) == 0 {
			b.add("maven", base+"/"+component.Name+"-"+component.Version+".jar", false, 0, path)
			continue
		}
		for _, artifact := range component.Artifacts {
			name := artifact.Name
			if name == "" {
				continue
			}
			b.add("maven", base+"/"+name, false, 0, path)
		}
	}
	return nil
}

func mavenDirectory(group, artifact, version string) string {
	return "/" + strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/" + version
}

func (b *builder) mavenCoordinate(group, artifact, version, classifier, extension, source string) {
	if extension == "" {
		extension = "jar"
	}
	name := artifact + "-" + version
	if classifier != "" {
		name += "-" + classifier
	}
	base := mavenDirectory(group, artifact, version)
	b.add("maven", base+"/"+artifact+"-"+version+".pom", true, 0, source)
	b.add("maven", base+"/"+name+"."+extension, false, 0, source)
}

func pythonRequirementName(name string) string {
	if index := strings.IndexByte(name, '['); index >= 0 {
		name = name[:index]
	}
	return strings.TrimSpace(name)
}

func normalizePythonName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, ".", "-")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	return name
}

func pythonIndexPath(name string) string { return "/simple/" + name + "/" }

func (b *builder) stripMount(pathValue string) string {
	for _, repo := range b.opts.Repos {
		mount := "/" + strings.Trim(repo.Mount, "/")
		if mount != "/" && strings.HasPrefix(pathValue, mount+"/") {
			return strings.TrimPrefix(pathValue, mount)
		}
	}
	return pathValue
}

func (b *builder) pythonFileURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "/packages/") {
		return "/_files/files.pythonhosted.org" + raw, true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Path == "" {
		return "", false
	}
	pathValue := b.stripMount(parsed.EscapedPath())
	if strings.HasPrefix(pathValue, "/_files/") {
		return pathValue, true
	}
	return "/_files/" + strings.ToLower(parsed.Host) + pathValue, true
}

var aptURLPattern = regexp.MustCompile(`https?://[^'"]+`)

func (b *builder) parseAptURIs(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		raw := aptURLPattern.FindString(scanner.Text())
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Path == "" {
			continue
		}
		repo, key, ok := b.matchAptURL(parsed)
		if !ok {
			b.warn("apt URI %s does not match a configured repository", raw)
			continue
		}
		b.addNamed(repo, key, false, 0, path)
	}
	return scanner.Err()
}

func (b *builder) matchAptURL(parsed *url.URL) (Repo, string, bool) {
	for _, repo := range b.opts.Repos {
		mount := "/" + strings.Trim(repo.Mount, "/")
		if mount != "/" && strings.HasPrefix(parsed.Path, mount+"/") {
			return repo, strings.TrimPrefix(parsed.Path, mount), true
		}
	}
	for _, repo := range b.opts.Repos {
		for _, upstream := range b.upstreamPaths(repo, parsed.Host) {
			if upstream == "/" || upstream == "" {
				return repo, parsed.Path, true
			}
			if strings.HasPrefix(parsed.Path, upstream+"/") {
				return repo, strings.TrimPrefix(parsed.Path, upstream), true
			}
		}
	}
	return Repo{}, "", false
}

func (b *builder) upstreamPaths(repo Repo, host string) []string {
	var out []string
	for _, configured := range b.repoUpstreams[repo.Name] {
		parsed, err := url.Parse(configured)
		if err != nil || !strings.EqualFold(parsed.Host, host) {
			continue
		}
		out = append(out, strings.TrimSuffix(parsed.Path, "/"))
	}
	return out
}
