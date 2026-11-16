package storage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/mmrzaf/larder/internal/index"
)

// ClassTotal is the current object and distinct blob total for one repo and class.
type ClassTotal struct {
	Repo        string `json:"repo"`
	Class       string `json:"class"`
	Objects     int64  `json:"objects"`
	Blobs       int64  `json:"blobs"`
	StoredBytes int64  `json:"stored_bytes"`
}

// RepoTotal is the aggregate current usage of one repository.
type RepoTotal struct {
	Repo        string `json:"repo"`
	Objects     int64  `json:"objects"`
	Blobs       int64  `json:"blobs"`
	StoredBytes int64  `json:"stored_bytes"`
}

// Stats is the storage summary returned by larder stats and the admin API.
type Stats struct {
	Usage
	ByClass []ClassTotal `json:"by_class"`
	ByRepo  []RepoTotal  `json:"repos"`
}

// Stats returns the current accounting and per-repo breakdown.
func (m *Manager) Stats(ctx context.Context) (Stats, error) {
	usage, err := m.Usage(ctx, true)
	if err != nil {
		return Stats{}, err
	}
	counts, err := m.index.ObjectCounts(ctx)
	if err != nil {
		return Stats{}, err
	}
	totals, err := m.index.RepoClassTotals(ctx)
	if err != nil {
		return Stats{}, err
	}
	byClass := make([]ClassTotal, 0, len(counts))
	for _, count := range counts {
		total := ClassTotal{Repo: count.Repo, Class: count.Class, Objects: count.Objects}
		for _, repoTotal := range totals {
			if repoTotal.Repo == count.Repo && repoTotal.Class == count.Class {
				total.Blobs = repoTotal.Blobs
				total.StoredBytes = repoTotal.StoredBytes
			}
		}
		byClass = append(byClass, total)
	}
	repoIndex := make(map[string]int)
	var byRepo []RepoTotal
	for _, total := range totals {
		position, ok := repoIndex[total.Repo]
		if !ok {
			position = len(byRepo)
			repoIndex[total.Repo] = position
			byRepo = append(byRepo, RepoTotal{Repo: total.Repo})
		}
		byRepo[position].Blobs += total.Blobs
		byRepo[position].StoredBytes += total.StoredBytes
	}
	for _, count := range counts {
		byRepo[repoIndex[count.Repo]].Objects += count.Objects
	}
	sort.Slice(byRepo, func(i, j int) bool { return byRepo[i].Repo < byRepo[j].Repo })
	return Stats{Usage: usage, ByClass: byClass, ByRepo: byRepo}, nil
}

// AgeBucket is one last-access age range.
type AgeBucket struct {
	Label   string `json:"label"`
	Objects int64  `json:"objects"`
	Bytes   int64  `json:"bytes"`
}

// DUResult is a usage breakdown.
type DUResult struct {
	By      string       `json:"by"`
	Classes []ClassTotal `json:"classes,omitempty"`
	Repos   []RepoTotal  `json:"repos,omitempty"`
	Ages    []AgeBucket  `json:"ages,omitempty"`
	Top     []ObjectView `json:"top,omitempty"`
}

// DU groups usage by repo, class, last-access age or largest objects.
func (m *Manager) DU(ctx context.Context, by string, n int) (DUResult, error) {
	switch by {
	case "repo":
		stats, err := m.Stats(ctx)
		if err != nil {
			return DUResult{}, err
		}
		return DUResult{By: by, Repos: stats.ByRepo}, nil
	case "class":
		stats, err := m.Stats(ctx)
		if err != nil {
			return DUResult{}, err
		}
		return DUResult{By: by, Classes: stats.ByClass}, nil
	case "age":
		objects, err := m.index.CurrentObjects(ctx, "")
		if err != nil {
			return DUResult{}, err
		}
		return DUResult{By: by, Ages: ageBuckets(m.clock.Now(), objects)}, nil
	case "top":
		objects, err := m.index.CurrentObjects(ctx, "")
		if err != nil {
			return DUResult{}, err
		}
		matcher, err := m.loadPins(ctx)
		if err != nil {
			return DUResult{}, err
		}
		sort.SliceStable(objects, func(i, j int) bool {
			if objects[i].StoredSize != objects[j].StoredSize {
				return objects[i].StoredSize > objects[j].StoredSize
			}
			return objects[i].Key < objects[j].Key
		})
		if n <= 0 {
			n = 50
		}
		if len(objects) > n {
			objects = objects[:n]
		}
		out := make([]ObjectView, 0, len(objects))
		for _, object := range objects {
			out = append(out, objectView(object, matcher))
		}
		return DUResult{By: by, Top: out}, nil
	default:
		return DUResult{}, fmt.Errorf("du: unknown grouping %q", by)
	}
}

var ageBoundaries = []struct {
	label string
	days  int
}{
	{"1", 1}, {"7", 7}, {"30", 30}, {"90", 90}, {"180", 180}, {"365", 365},
}

func ageBuckets(now time.Time, objects []index.Candidate) []AgeBucket {
	buckets := make([]AgeBucket, len(ageBoundaries)+1)
	for i, boundary := range ageBoundaries {
		buckets[i].Label = boundary.label
	}
	buckets[len(buckets)-1].Label = "older"
	for _, object := range objects {
		accessed := object.LastAccess
		if accessed.IsZero() {
			accessed = object.FetchedAt
		}
		days := now.Sub(accessed).Hours() / 24
		position := len(ageBoundaries)
		for i, boundary := range ageBoundaries {
			if days <= float64(boundary.days) {
				position = i
				break
			}
		}
		buckets[position].Objects++
		buckets[position].Bytes += object.StoredSize
	}
	return buckets
}

// ListOptions filters and orders object listings.
type ListOptions struct {
	Repo           string
	Match          string
	Sort           string
	Limit          int
	Offset         int
	PinnedOnly     bool
	UnverifiedOnly bool
}

// List returns current objects matching the options.
func (m *Manager) List(ctx context.Context, opts ListOptions) ([]ObjectView, error) {
	objects, err := m.index.CurrentObjects(ctx, opts.Repo)
	if err != nil {
		return nil, err
	}
	matcher, err := m.loadPins(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]index.Candidate, 0, len(objects))
	for _, object := range objects {
		if opts.Match != "" && !pinMatch(opts.Match, object.Key) {
			continue
		}
		pinned := matcher.pinned(object.Repo, object.Key)
		if opts.PinnedOnly && !pinned {
			continue
		}
		if opts.UnverifiedOnly && object.Verified {
			continue
		}
		filtered = append(filtered, object)
	}
	switch opts.Sort {
	case "size":
		sort.SliceStable(filtered, func(i, j int) bool {
			if filtered[i].StoredSize != filtered[j].StoredSize {
				return filtered[i].StoredSize > filtered[j].StoredSize
			}
			return filtered[i].Key < filtered[j].Key
		})
	case "atime":
		sort.SliceStable(filtered, func(i, j int) bool {
			if !filtered[i].LastAccess.Equal(filtered[j].LastAccess) {
				return filtered[i].LastAccess.After(filtered[j].LastAccess)
			}
			return filtered[i].Key < filtered[j].Key
		})
	case "hits":
		sort.SliceStable(filtered, func(i, j int) bool {
			if filtered[i].Hits != filtered[j].Hits {
				return filtered[i].Hits > filtered[j].Hits
			}
			return filtered[i].Key < filtered[j].Key
		})
	default:
		sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Key < filtered[j].Key })
	}
	if opts.Offset > 0 {
		if opts.Offset >= len(filtered) {
			return []ObjectView{}, nil
		}
		filtered = filtered[opts.Offset:]
	}
	if opts.Limit > 0 && len(filtered) > opts.Limit {
		filtered = filtered[:opts.Limit]
	}
	out := make([]ObjectView, 0, len(filtered))
	for _, object := range filtered {
		out = append(out, objectView(object, matcher))
	}
	return out, nil
}

// ObjectView is one object in a listing.
type ObjectView struct {
	Repo       string    `json:"repo"`
	Key        string    `json:"key"`
	Class      string    `json:"class"`
	Generation int64     `json:"generation"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"stored_size"`
	RawSize    int64     `json:"raw_size"`
	Verified   bool      `json:"verified"`
	Pinned     bool      `json:"pinned"`
	LastAccess time.Time `json:"last_access"`
	Hits       int64     `json:"hits"`
	StoredEnc  string    `json:"stored_enc"`
}

func objectView(object index.Candidate, matcher *pinMatcher) ObjectView {
	return ObjectView{
		Repo: object.Repo, Key: object.Key, Class: object.Class,
		Generation: object.Generation, SHA256: object.SHA256,
		Size: object.StoredSize, RawSize: object.RawSize,
		Verified: object.Verified, Pinned: matcher.pinned(object.Repo, object.Key),
		LastAccess: object.LastAccess, Hits: object.Hits, StoredEnc: object.StoredEnc,
	}
}

// ExpectationView is one recorded digest expectation.
type ExpectationView struct {
	Algo      string    `json:"algo"`
	Digest    string    `json:"digest"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// ObjectDetails describes one key and optionally its generations.
type ObjectDetails struct {
	Object       ObjectView        `json:"object"`
	Blob         BlobView          `json:"blob"`
	Expectations []ExpectationView `json:"expectations"`
	Pins         []string          `json:"pins"`
	Generations  []ObjectView      `json:"generations,omitempty"`
}

// BlobView is the blob row behind an object.
type BlobView struct {
	SHA256     string    `json:"sha256"`
	RawSize    int64     `json:"raw_size"`
	StoredSize int64     `json:"stored_size"`
	StoredEnc  string    `json:"stored_enc"`
	RefCount   int64     `json:"refcount"`
	CreatedAt  time.Time `json:"created_at"`
}

// Details returns one key with current blob state, expectations and pins.
func (m *Manager) Details(ctx context.Context, repo, key string, history bool) (ObjectDetails, error) {
	current, err := m.index.CurrentObject(ctx, repo, key)
	if err != nil {
		return ObjectDetails{}, err
	}
	matcher, err := m.loadPins(ctx)
	if err != nil {
		return ObjectDetails{}, err
	}
	blob, err := m.index.GetBlob(ctx, current.SHA256)
	if err != nil {
		return ObjectDetails{}, err
	}
	expectations, err := m.index.ExpectationsFor(ctx, repo, key)
	if err != nil {
		return ObjectDetails{}, err
	}
	generations := []index.Object{current}
	if history {
		generations, err = m.index.Generations(ctx, repo, key)
		if err != nil {
			return ObjectDetails{}, err
		}
	}
	views := make([]ObjectView, 0, len(generations))
	for _, object := range generations {
		views = append(views, objectView(index.Candidate{Object: object}, matcher))
	}
	expectationViews := make([]ExpectationView, 0, len(expectations))
	for _, expectation := range expectations {
		expectationViews = append(expectationViews, ExpectationView{
			Algo: expectation.Algo, Digest: expectation.Digest,
			Source: expectation.Source, CreatedAt: expectation.CreatedAt,
		})
	}
	return ObjectDetails{
		Object: objectView(index.Candidate{Object: current}, matcher),
		Blob: BlobView{
			SHA256: blob.SHA256, RawSize: blob.RawSize, StoredSize: blob.StoredSize,
			StoredEnc: blob.StoredEnc, RefCount: blob.RefCount, CreatedAt: blob.CreatedAt,
		},
		Expectations: expectationViews,
		Pins:         matcher.sets(repo, key),
		Generations:  views,
	}, nil
}

// PinSetSummary is one pin set with its matched content.
type PinSetSummary struct {
	Name           string    `json:"name"`
	Source         string    `json:"source"`
	Note           string    `json:"note,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	Items          int64     `json:"items"`
	Rules          int64     `json:"rules"`
	MatchedObjects int64     `json:"matched_objects"`
	Bytes          int64     `json:"bytes"`
}

// Pins lists stored and declarative pin sets with matched bytes.
func (m *Manager) Pins(ctx context.Context) ([]PinSetSummary, error) {
	cfg := m.cfg.Load()
	if cfg == nil {
		return nil, fmt.Errorf("storage: no configuration")
	}
	sets, err := m.index.ListPinSets(ctx)
	if err != nil {
		return nil, err
	}
	matcher, err := m.loadPins(ctx)
	if err != nil {
		return nil, err
	}
	objects, err := m.index.CurrentObjects(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]PinSetSummary, 0, len(sets)+1)
	for _, set := range sets {
		summary := PinSetSummary{
			Name: set.Name, Source: "pin", Note: set.Note, CreatedAt: set.CreatedAt,
			Items: set.Items, Rules: set.Rules,
		}
		seen := make(map[string]bool)
		for _, object := range objects {
			if !matcher.matchesSet(object.Repo, object.Key, set.Name) {
				continue
			}
			summary.MatchedObjects++
			if !seen[object.SHA256] {
				seen[object.SHA256] = true
				summary.Bytes += object.StoredSize
			}
		}
		out = append(out, summary)
	}
	configRules := int64(len(cfg.Storage.Protect))
	for i := range cfg.Repos {
		configRules += int64(len(cfg.Repos[i].Protect))
	}
	if configRules > 0 {
		summary := PinSetSummary{Name: "config", Source: "config", Rules: configRules}
		seen := make(map[string]bool)
		for _, object := range objects {
			if !matcher.matchesSet(object.Repo, object.Key, "config") {
				continue
			}
			summary.MatchedObjects++
			if !seen[object.SHA256] {
				seen[object.SHA256] = true
				summary.Bytes += object.StoredSize
			}
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
