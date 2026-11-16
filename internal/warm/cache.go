package warm

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
)

func (b *builder) fromCache(spec string) error {
	kind, path, hasPath := strings.Cut(spec, "=")
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		return fmt.Errorf("cache kind is required")
	}
	if !hasPath || strings.TrimSpace(path) == "" {
		path = defaultCachePath(kind)
	}
	path = expandHome(strings.TrimSpace(path))
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	switch kind {
	case "go":
		return b.fromGoCache(path)
	case "maven":
		return b.fromMavenCache(path)
	case "gradle":
		return b.fromGradleCache(path)
	case "cargo":
		return b.fromCargoCache(path)
	default:
		return fmt.Errorf("unsupported cache kind %q", kind)
	}
}

func defaultCachePath(kind string) string {
	home, _ := os.UserHomeDir()
	switch kind {
	case "go":
		if gopath := os.Getenv("GOPATH"); gopath != "" {
			return filepath.Join(gopath, "pkg", "mod", "cache", "download")
		}
		return filepath.Join(home, "go", "pkg", "mod", "cache", "download")
	case "maven":
		return filepath.Join(home, ".m2", "repository")
	case "gradle":
		return filepath.Join(home, ".gradle", "caches", "modules", "files-2.1")
	case "cargo":
		return filepath.Join(home, ".cargo", "registry", "cache")
	default:
		return ""
	}
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func (b *builder) fromGoCache(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name := entry.Name()
		ext := filepath.Ext(name)
		if ext != ".info" && ext != ".mod" && ext != ".zip" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		index := strings.LastIndex(rel, "/@v/")
		if index < 0 {
			return nil
		}
		escapedModule := rel[:index]
		if _, err := module.UnescapePath(escapedModule); err != nil {
			return nil
		}
		version := strings.TrimSuffix(name, ext)
		if _, err := module.UnescapeVersion(version); err != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		b.add("go", "/"+escapedModule+"/@v/"+version+ext, ext != ".zip", info.Size(), path)
		return nil
	})
}

func (b *builder) fromMavenCache(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name := entry.Name()
		if ignoredCacheFile(name) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		metadata := strings.HasSuffix(name, ".pom") || strings.HasPrefix(name, "maven-metadata")
		b.add("maven", "/"+filepath.ToSlash(rel), metadata, info.Size(), path)
		return nil
	})
}

func ignoredCacheFile(name string) bool {
	switch {
	case strings.HasSuffix(name, ".lastUpdated"),
		strings.HasSuffix(name, ".sha1"), strings.HasSuffix(name, ".md5"),
		strings.HasSuffix(name, ".sha256"), strings.HasSuffix(name, ".sha512"),
		name == "_remote.repositories", name == "resolver-status.properties":
		return true
	default:
		return false
	}
}

func (b *builder) fromGradleCache(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 5 {
			return nil
		}
		group := parts[0]
		artifact := parts[1]
		version := parts[2]
		file := parts[len(parts)-1]
		if ignoredCacheFile(file) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		metadata := strings.HasSuffix(file, ".pom")
		b.add("maven", "/"+group+"/"+artifact+"/"+version+"/"+file, metadata, info.Size(), path)
		return nil
	})
}

var crateFilePattern = regexp.MustCompile(`^(.+)-([0-9][^/]*)\.crate$`)

func (b *builder) fromCargoCache(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		match := crateFilePattern.FindStringSubmatch(entry.Name())
		if len(match) != 3 {
			return nil
		}
		name, version := match[1], match[2]
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		b.add("crates", "/index/"+crateIndexPath(name), true, 0, path)
		b.add("crates", "/dl/"+name+"/"+version+"/download", false, info.Size(), path)
		return nil
	})
}

func (b *builder) urlList(path, repoName string) error {
	if repoName == "" {
		return fmt.Errorf("--repo is required with --url-list")
	}
	var target Repo
	found := false
	for _, repo := range b.opts.Repos {
		if repo.Name == repoName {
			target = repo
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("unknown repo %q", repoName)
	}
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
		b.addNamed(target, "/"+strings.TrimPrefix(line, "/"), false, 0, path)
	}
	return scanner.Err()
}

func (b *builder) addNamed(repo Repo, path string, metadata bool, size int64, source string) {
	if len(b.only) > 0 && !b.only[repo.Kind] {
		return
	}
	path = "/" + strings.TrimPrefix(path, "/")
	key := repo.Name + "\x00" + path
	if index, exists := b.seen[key]; exists {
		if metadata {
			b.items[index].Metadata = true
		}
		return
	}
	b.seen[key] = len(b.items)
	b.items = append(b.items, Item{
		Kind: repo.Kind, Repo: repo.Name, Path: path, Metadata: metadata, Size: size, Source: source,
	})
}
