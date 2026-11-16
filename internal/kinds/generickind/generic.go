// Package generickind implements the generic caching kind.
package generickind

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
)

// Kind is the generic repository classifier.
type Kind struct {
	opts       config.GenericOptions
	rules      []compiledRule
	expect     map[string][]integrity.Digest
	companions []kinds.Companion
}

type compiledRule struct {
	match *regexp.Regexp
	class kinds.Class
	fresh time.Duration
}

// New builds a generic kind from repository configuration.
func New(repo config.Repo) (kinds.Kind, error) {
	if repo.Generic == nil {
		return nil, fmt.Errorf("generic repo %s has no generic options", repo.Name)
	}
	opts := *repo.Generic
	k := &Kind{
		opts:       opts,
		expect:     make(map[string][]integrity.Digest),
		companions: make([]kinds.Companion, 0, len(opts.Companion)),
	}
	for i, rule := range opts.Rules {
		class, err := classFor(rule.Class)
		if err != nil {
			return nil, fmt.Errorf("generic repo %s rules[%d]: %w", repo.Name, i, err)
		}
		compiled, err := regexp.Compile(rule.Match)
		if err != nil {
			return nil, fmt.Errorf("generic repo %s rules[%d]: %w", repo.Name, i, err)
		}
		var fresh time.Duration
		if rule.FreshTTL != nil {
			fresh = rule.FreshTTL.Duration()
		}
		k.rules = append(k.rules, compiledRule{match: compiled, class: class, fresh: fresh})
	}
	for i, e := range opts.Expect {
		var digests []integrity.Digest
		for _, item := range []struct {
			algo  string
			value string
		}{
			{"sha256", e.SHA256},
			{"sha512", e.SHA512},
			{"sha1", e.SHA1},
		} {
			if item.value == "" {
				continue
			}
			digest, err := integrity.ParseHex(item.algo, item.value)
			if err != nil {
				return nil, fmt.Errorf("generic repo %s expect[%d]: %w", repo.Name, i, err)
			}
			digests = append(digests, digest)
		}
		k.expect[e.Path] = digests
	}
	for i, companion := range opts.Companion {
		if companion.Suffix == "" || companion.Algo == "" {
			return nil, fmt.Errorf("generic repo %s companion[%d] is incomplete", repo.Name, i)
		}
		k.companions = append(k.companions, kinds.Companion{Suffix: companion.Suffix, Algo: companion.Algo})
	}
	return k, nil
}

// Name returns the kind name.
func (k *Kind) Name() string { return "generic" }

// Classify applies the first matching rule, or the configured default class.
func (k *Kind) Classify(req kinds.Request) (kinds.Plan, error) {
	class, fresh := k.defaultClass(), time.Duration(0)
	for _, rule := range k.rules {
		if rule.match.MatchString(req.Path) {
			class = rule.class
			if class == kinds.Mutable {
				fresh = rule.fresh
			}
			break
		}
	}

	key := req.Path
	upstreamPath := req.Path
	if req.RawQuery != "" {
		upstreamPath += "?" + req.RawQuery
		if k.opts.KeepQuery {
			key += "?" + req.RawQuery
		}
	}
	plan := kinds.Plan{
		Class:        class,
		Key:          key,
		UpstreamPath: upstreamPath,
		FreshTTL:     fresh,
	}
	if class == kinds.Immutable && k.opts.RejectHTML && !strings.HasSuffix(strings.ToLower(req.Path), ".html") {
		plan.Binary = true
	}
	if class == kinds.Immutable {
		plan.Hooks.Expect = k.expectFor
		plan.Hooks.Companions = k.companionsFor
	}
	return plan, nil
}

func (k *Kind) defaultClass() kinds.Class {
	switch k.opts.DefaultClass {
	case "mutable":
		return kinds.Mutable
	default:
		return kinds.Deny
	}
}

func (k *Kind) expectFor(key string) []integrity.Digest {
	path, _, _ := strings.Cut(key, "?")
	return append([]integrity.Digest(nil), k.expect[path]...)
}

func (k *Kind) companionsFor(string) []kinds.Companion {
	return append([]kinds.Companion(nil), k.companions...)
}

func classFor(value string) (kinds.Class, error) {
	switch value {
	case "immutable":
		return kinds.Immutable, nil
	case "mutable":
		return kinds.Mutable, nil
	case "deny":
		return kinds.Deny, nil
	default:
		return 0, fmt.Errorf("unknown class %q", value)
	}
}

var _ kinds.Kind = (*Kind)(nil)
