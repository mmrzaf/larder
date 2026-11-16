// Package alpinekind implements Alpine Linux package repositories for apk.
package alpinekind

import (
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
)

const indexName = "APKINDEX.tar.gz"

// Kind classifies Alpine repository paths of the form
// /<branch>/<repository>/<architecture>/<file>.
//
// Package files and indexes are passed through byte for byte. apk verifies the
// index signature against the keys installed on the client and each package
// against the checksum in the index, so Larder adds no verification of its own.
type Kind struct{}

// New builds an Alpine kind from repository configuration.
func New(config.Repo) (kinds.Kind, error) { return &Kind{}, nil }

// Name returns the kind name.
func (k *Kind) Name() string { return "alpine" }

// Classify maps an Alpine path to a plan.
func (k *Kind) Classify(req kinds.Request) (kinds.Plan, error) {
	plan := kinds.Plan{Key: req.Path, UpstreamPath: req.Path, Class: kinds.Deny}
	parts := strings.Split(strings.TrimPrefix(req.Path, "/"), "/")
	if len(parts) != 4 || !branch(parts[0]) || !segment(parts[1]) || !segment(parts[2]) {
		return plan, nil
	}
	switch file := parts[3]; {
	case file == indexName:
		plan.Class = kinds.Mutable
		plan.FreshTTL = 300 * time.Second
		plan.Binary = true
	case strings.HasSuffix(file, ".apk") && packageFile(file):
		plan.Class = kinds.Immutable
		plan.Binary = true
	}
	return plan, nil
}

// branch accepts release branches (v3.20), edge and the latest-stable alias.
func branch(value string) bool {
	if value == "edge" || value == "latest-stable" {
		return true
	}
	if len(value) < 4 || value[0] != 'v' {
		return false
	}
	dots := 0
	for _, r := range value[1:] {
		switch {
		case r == '.':
			dots++
		case r < '0' || r > '9':
			return false
		}
	}
	return dots == 1 && value[len(value)-1] != '.' && value[1] != '.'
}

func segment(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') || value == ".." {
			return false
		}
	}
	return value != "." && value != ".."
}

func packageFile(value string) bool {
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.+~", r)) {
			return false
		}
	}
	return !strings.HasPrefix(value, ".")
}

var _ kinds.Kind = (*Kind)(nil)
