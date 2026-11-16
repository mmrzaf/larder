package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
)

type pinRule struct {
	set     string
	repo    string
	pattern string
}

type pinMatcher struct {
	items map[string]map[string][]string
	rules []pinRule
}

func (m *Manager) loadPins(ctx context.Context) (*pinMatcher, error) {
	cfg := m.cfg.Load()
	if cfg == nil {
		return nil, fmt.Errorf("storage: no configuration")
	}
	matcher := &pinMatcher{items: make(map[string]map[string][]string)}
	items, err := m.index.PinItems(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if matcher.items[item.Repo] == nil {
			matcher.items[item.Repo] = make(map[string][]string)
		}
		matcher.items[item.Repo][item.Key] = append(matcher.items[item.Repo][item.Key], item.SetName)
	}
	rules, err := m.index.PinRules(ctx)
	if err != nil {
		return nil, err
	}
	for _, rule := range rules {
		matcher.rules = append(matcher.rules, pinRule{
			set: rule.SetName, repo: rule.Repo, pattern: rule.Pattern,
		})
	}
	for _, protect := range cfg.Storage.Protect {
		matcher.rules = append(matcher.rules, pinRule{
			set: "config", repo: protect.Repo, pattern: protect.Match,
		})
	}
	for i := range cfg.Repos {
		for _, pattern := range cfg.Repos[i].Protect {
			matcher.rules = append(matcher.rules, pinRule{
				set: "config", repo: cfg.Repos[i].Name, pattern: pattern,
			})
		}
	}
	return matcher, nil
}

func (p *pinMatcher) pinned(repo, key string) bool {
	if len(p.items[repo][key]) > 0 {
		return true
	}
	for _, rule := range p.rules {
		if rule.repo == repo && pinMatch(rule.pattern, key) {
			return true
		}
	}
	return false
}

func (p *pinMatcher) sets(repo, key string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, set := range p.items[repo][key] {
		if !seen[set] {
			seen[set] = true
			out = append(out, set)
		}
	}
	for _, rule := range p.rules {
		if rule.repo == repo && pinMatch(rule.pattern, key) && !seen[rule.set] {
			seen[rule.set] = true
			out = append(out, rule.set)
		}
	}
	return out
}

func pinMatch(pattern, key string) bool {
	if !strings.Contains(pattern, "#") {
		if index := strings.IndexByte(key, '#'); index >= 0 {
			key = key[:index]
		}
	}
	if index.MatchGlob(pattern, key) {
		return true
	}
	return len(key) > 1 && key[0] == '/' && index.MatchGlob(pattern, key[1:])
}

func (p *pinMatcher) matchesSet(repo, key, set string) bool {
	if set == "config" {
		for _, rule := range p.rules {
			if rule.set == "config" && rule.repo == repo && pinMatch(rule.pattern, key) {
				return true
			}
		}
		return false
	}
	for _, name := range p.items[repo][key] {
		if name == set {
			return true
		}
	}
	for _, rule := range p.rules {
		if rule.set == set && rule.repo == repo && pinMatch(rule.pattern, key) {
			return true
		}
	}
	return false
}

func (m *Manager) pinnedBytes(ctx context.Context, cfg *config.Config) (int64, error) {
	matcher, err := m.loadPins(ctx)
	if err != nil {
		return 0, err
	}
	objects, err := m.index.CurrentObjects(ctx, "")
	if err != nil {
		return 0, err
	}
	seen := make(map[string]bool)
	var total int64
	for _, object := range objects {
		if !matcher.pinned(object.Repo, object.Key) || seen[object.SHA256] {
			continue
		}
		seen[object.SHA256] = true
		total += object.StoredSize
	}
	return total, nil
}

// AddPinRequest adds an exact key or a glob rule to a pin set.
type AddPinRequest struct {
	Set   string
	Repo  string
	Key   string
	Match string
	Note  string
}

// AddPin creates or extends a pin set.
func (m *Manager) AddPin(ctx context.Context, req AddPinRequest) error {
	if req.Set == "" {
		return fmt.Errorf("pin: set name is required")
	}
	if req.Repo == "" {
		return fmt.Errorf("pin: repo is required")
	}
	if (req.Key == "") == (req.Match == "") {
		return fmt.Errorf("pin: exactly one of key or match is required")
	}
	err := m.index.CreatePinSet(ctx, index.PinSet{Name: req.Set, CreatedAt: m.clock.Now(), Note: req.Note})
	if err != nil && !errors.Is(err, index.ErrExists) {
		return err
	}
	if req.Key != "" {
		return m.index.AddPinItem(ctx, index.PinItem{SetName: req.Set, Repo: req.Repo, Key: req.Key})
	}
	return m.index.AddPinRule(ctx, index.PinRule{SetName: req.Set, Repo: req.Repo, Pattern: req.Match})
}

// RemovePin deletes a rule when match is set and the whole set otherwise.
func (m *Manager) RemovePin(ctx context.Context, set, repo, match string) error {
	if set == "" {
		return fmt.Errorf("unpin: set name is required")
	}
	if match == "" {
		return m.index.DeletePinSet(ctx, set)
	}
	if repo == "" {
		return fmt.Errorf("unpin: repo is required with match")
	}
	return m.index.RemovePinRule(ctx, index.PinRule{SetName: set, Repo: repo, Pattern: match})
}

// IsPinned reports whether a key matches a stored or declarative pin.
func (m *Manager) IsPinned(ctx context.Context, repo, key string) (bool, error) {
	matcher, err := m.loadPins(ctx)
	if err != nil {
		return false, err
	}
	return matcher.pinned(repo, key), nil
}
