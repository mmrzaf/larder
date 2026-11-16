// Package mavenkind implements the Maven repository layout used by Maven,
// Gradle, Kotlin, sbt and Ivy resolvers.
package mavenkind

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
)

const (
	metadataTTL = 300 * time.Second
)

var companionOrder = []kinds.Companion{
	{Suffix: ".sha512", Algo: "sha512"},
	{Suffix: ".sha256", Algo: "sha256"},
	{Suffix: ".sha1", Algo: "sha1"},
	{Suffix: ".md5", Algo: "md5"},
}

var checksumAlgorithms = map[string]string{
	".sha512": "sha512",
	".sha256": "sha256",
	".sha1":   "sha1",
	".md5":    "md5",
}

// Kind classifies Maven repository paths.
type Kind struct {
	opts config.MavenOptions
}

// New builds a Maven kind from repository configuration.
func New(repo config.Repo) (kinds.Kind, error) {
	if repo.Maven == nil {
		return nil, fmt.Errorf("maven repo %s has no maven options", repo.Name)
	}
	return &Kind{opts: *repo.Maven}, nil
}

// Name returns the kind name.
func (k *Kind) Name() string { return "maven" }

// Classify maps a Maven repository path to a plan.
func (k *Kind) Classify(req kinds.Request) (kinds.Plan, error) {
	pathValue := req.Path
	plan := kinds.Plan{Key: pathValue, UpstreamPath: pathValue}
	if strings.HasSuffix(pathValue, "/") {
		plan.Class = kinds.Deny
		return plan, nil
	}
	base := path.Base(pathValue)
	switch {
	case strings.HasPrefix(base, "maven-metadata"):
		plan.Class = kinds.Mutable
		plan.FreshTTL = metadataTTL
		return plan, nil
	case strings.HasPrefix(base, "archetype-catalog.xml"):
		plan.Class = kinds.Mutable
		return plan, nil
	case snapshotMutable(pathValue, base):
		plan.Class = kinds.Mutable
		return plan, nil
	}

	plan.Class = kinds.Immutable
	plan.Binary = true
	if k.opts.CompanionFetch && !isChecksumOrSignature(base) {
		plan.Hooks.Companions = companions
	}
	plan.Hooks.ChecksumFor = checksumFor
	return plan, nil
}

func snapshotMutable(pathValue, base string) bool {
	versionDir := path.Base(path.Dir(pathValue))
	return strings.HasSuffix(versionDir, "-SNAPSHOT") && strings.Contains(base, "-SNAPSHOT")
}

func isChecksumOrSignature(base string) bool {
	if _, ok := checksumAlgorithms[path.Ext(base)]; ok {
		return true
	}
	return strings.HasSuffix(base, ".asc")
}

func companions(string) []kinds.Companion {
	out := make([]kinds.Companion, len(companionOrder))
	copy(out, companionOrder)
	return out
}

// checksumFor reports the artifact a checksum file belongs to and its algorithm.
func checksumFor(key string) (string, string, bool) {
	ext := path.Ext(key)
	algo, ok := checksumAlgorithms[ext]
	if !ok {
		return "", "", false
	}
	return strings.TrimSuffix(key, ext), algo, true
}

var _ kinds.Kind = (*Kind)(nil)
