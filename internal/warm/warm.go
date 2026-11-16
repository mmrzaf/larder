// Package warm discovers client lockfiles, builds request plans and fetches
// them through a running Larder server.
package warm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Repo identifies one configured repository as seen by the CLI.
type Repo struct {
	Name  string
	Kind  string
	Mount string
}

// Options controls plan construction.
type Options struct {
	Paths     []string
	Repos     []Repo
	RepoMap   map[string]string
	Only      []string
	FromCache []string
	URLList   string
	URLRepo   string
	Platform  string
	Upstreams []RepoUpstream
}

// RepoUpstream is one configured upstream URL for URI-list matching.
type RepoUpstream struct {
	Repo string
	URL  string
}

// Item is one request in a plan.
type Item struct {
	Kind     string `json:"kind"`
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	Metadata bool   `json:"metadata"`
	Size     int64  `json:"size,omitempty"`
	Source   string `json:"source,omitempty"`
}

// Plan is a deduplicated ordered request list.
type Plan struct {
	Items    []Item   `json:"items"`
	Warnings []string `json:"warnings,omitempty"`
	Skipped  []string `json:"skipped,omitempty"`
}

type builder struct {
	opts          Options
	repos         map[string]Repo
	items         []Item
	seen          map[string]int
	warnings      []string
	skipped       []string
	only          map[string]bool
	repoUpstreams map[string][]string
}

// Build discovers files and produces a request plan.
func Build(opts Options) (Plan, error) {
	b := &builder{
		opts: opts, repos: make(map[string]Repo),
		seen: make(map[string]int), only: make(map[string]bool),
	}
	for _, kind := range opts.Only {
		kind = strings.TrimSpace(kind)
		if kind != "" {
			b.only[kind] = true
		}
	}
	for _, repo := range opts.Repos {
		if _, exists := b.repos[repo.Kind]; !exists {
			b.repos[repo.Kind] = repo
		}
	}
	b.repoUpstreams = make(map[string][]string)
	for _, upstream := range opts.Upstreams {
		b.repoUpstreams[upstream.Repo] = append(b.repoUpstreams[upstream.Repo], upstream.URL)
	}
	for kind, name := range opts.RepoMap {
		found := false
		for _, repo := range opts.Repos {
			if repo.Name == name && repo.Kind == kind {
				b.repos[kind] = repo
				found = true
				break
			}
		}
		if !found {
			b.warn("repo-map: no %s repository named %s", kind, name)
		}
	}
	for _, path := range opts.Paths {
		if err := b.discover(path); err != nil {
			b.warn("%s: %v", path, err)
		}
	}
	for _, spec := range opts.FromCache {
		if err := b.fromCache(spec); err != nil {
			b.warn("from-cache %s: %v", spec, err)
		}
	}
	if opts.URLList != "" {
		if err := b.urlList(opts.URLList, opts.URLRepo); err != nil {
			b.warn("url-list: %v", err)
		}
	}
	plan := Plan{Items: b.items, Warnings: b.warnings, Skipped: b.skipped}
	sortItems(plan.Items, opts.Repos)
	return plan, nil
}

func (b *builder) add(kind, path string, metadata bool, size int64, source string) {
	if len(b.only) > 0 && !b.only[kind] {
		return
	}
	repo, ok := b.repos[kind]
	if !ok {
		b.warn("no repository configured for kind %s (%s)", kind, source)
		return
	}
	path = "/" + strings.TrimPrefix(path, "/")
	if path == "/" {
		return
	}
	key := repo.Name + "\x00" + path
	if index, exists := b.seen[key]; exists {
		if metadata {
			b.items[index].Metadata = true
		}
		if size > b.items[index].Size {
			b.items[index].Size = size
		}
		return
	}
	b.seen[key] = len(b.items)
	b.items = append(b.items, Item{
		Kind: kind, Repo: repo.Name, Path: path, Metadata: metadata, Size: size, Source: source,
	})
}

func (b *builder) skip(format string, args ...any) {
	b.skipped = append(b.skipped, fmt.Sprintf(format, args...))
}

func (b *builder) warn(format string, args ...any) {
	b.warnings = append(b.warnings, fmt.Sprintf(format, args...))
}

func sortItems(items []Item, repos []Repo) {
	order := make(map[string]int, len(repos))
	for i, repo := range repos {
		order[repo.Name] = i
	}
	sort.SliceStable(items, func(i, j int) bool {
		left, right := items[i], items[j]
		if order[left.Repo] != order[right.Repo] {
			return order[left.Repo] < order[right.Repo]
		}
		if left.Metadata != right.Metadata {
			return left.Metadata
		}
		return false
	})
}

var skipDirectories = map[string]bool{
	"node_modules": true, ".git": true, "vendor": true, "target": true,
	".venv": true, "dist": true, "build": true,
}

func (b *builder) discover(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return b.parseFile(path)
	}
	return filepath.WalkDir(path, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if current != path && skipDirectories[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		return b.parseFile(current)
	})
}

func (b *builder) parseFile(path string) error {
	name := filepath.Base(path)
	switch {
	case name == "go.sum":
		return b.parseGoSum(path)
	case name == "package-lock.json" || name == "npm-shrinkwrap.json":
		return b.parsePackageLock(path)
	case name == "pnpm-lock.yaml":
		return b.parsePNPMLock(path)
	case name == "yarn.lock":
		return b.parseYarnLock(path)
	case name == "bun.lock" || name == "bun.lockb":
		return b.parseBunLock(path)
	case strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt"):
		return b.parseRequirements(path)
	case name == "poetry.lock":
		return b.parsePoetryLock(path)
	case name == "uv.lock":
		return b.parseUVLock(path)
	case name == "Pipfile.lock":
		return b.parsePipfileLock(path)
	case name == "pdm.lock":
		return b.parsePDMLock(path)
	case name == "Cargo.lock":
		return b.parseCargoLock(path)
	case name == "gradle.lockfile":
		return b.parseGradleLockfile(path)
	case name == "verification-metadata.xml":
		return b.parseVerificationMetadata(path)
	case name == "apt-packages.txt" || name == "apt.txt" || strings.HasSuffix(name, ".uris"):
		return b.parseAptURIs(path)
	case name == "pom.xml":
		b.skip("%s: pom.xml is not resolved; use --from-cache or larder seed", path)
		return nil
	default:
		return nil
	}
}
