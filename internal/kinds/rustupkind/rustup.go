// Package rustupkind implements the rustup distribution and toolchain protocol.
package rustupkind

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
)

// Kind classifies rustup distribution paths.
type Kind struct {
	upstreams  []string
	parseLimit int64
}

// New builds a rustup kind from repository configuration.
func New(repo config.Repo) (kinds.Kind, error) {
	limit := int64(512 << 20)
	if repo.MaxParseBytes > 0 {
		limit = repo.MaxParseBytes.Bytes()
	}
	kind := &Kind{parseLimit: limit}
	for _, upstream := range repo.Upstreams {
		kind.upstreams = append(kind.upstreams, strings.TrimSuffix(upstream.URL, "/"))
	}
	return kind, nil
}

// Name returns the kind name.
func (k *Kind) Name() string { return "rustup" }

// Classify maps a rustup path to a plan.
func (k *Kind) Classify(req kinds.Request) (kinds.Plan, error) {
	pathValue := req.Path
	plan := kinds.Plan{Key: pathValue, UpstreamPath: pathValue}
	switch {
	case isChannelManifest(pathValue):
		plan.Class = kinds.Mutable
		plan.FreshTTL = 300 * time.Second
		plan.Binary = strings.HasSuffix(pathValue, ".asc")
		if strings.HasSuffix(pathValue, ".toml") {
			plan.Hooks.Ingest = k.ingest
		}
		plan.Hooks.ChecksumFor = checksumFor
		return plan, nil
	case isDatedPath(pathValue):
		plan.Class = kinds.Immutable
		plan.Binary = !strings.HasSuffix(pathValue, ".toml") &&
			!strings.HasSuffix(pathValue, ".toml.sha256") &&
			!strings.HasSuffix(pathValue, ".toml.asc")
		if strings.HasSuffix(pathValue, ".toml") {
			plan.Hooks.Ingest = k.ingest
		}
		plan.Hooks.ChecksumFor = checksumFor
		return plan, nil
	case isMutableRustupInit(pathValue):
		plan.Class = kinds.Mutable
		plan.FreshTTL = 300 * time.Second
		plan.Binary = !strings.HasSuffix(pathValue, ".sha256")
		plan.Hooks.ChecksumFor = checksumFor
		return plan, nil
	case isArchiveRustupInit(pathValue):
		plan.Class = kinds.Immutable
		plan.Binary = !strings.HasSuffix(pathValue, ".sha256")
		plan.Hooks.ChecksumFor = checksumFor
		return plan, nil
	default:
		plan.Class = kinds.Deny
		return plan, nil
	}
}

func isChannelManifest(pathValue string) bool {
	if !strings.HasPrefix(pathValue, "/dist/channel-rust-") {
		return false
	}
	return strings.HasSuffix(pathValue, ".toml") ||
		strings.HasSuffix(pathValue, ".toml.sha256") ||
		strings.HasSuffix(pathValue, ".toml.asc")
}

func isDatedPath(pathValue string) bool {
	parts := strings.Split(strings.TrimPrefix(pathValue, "/"), "/")
	if len(parts) < 3 || parts[0] != "dist" || parts[2] == "" {
		return false
	}
	date := parts[1]
	if len(date) != 10 || date[4] != '-' || date[7] != '-' {
		return false
	}
	for i, r := range date {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isMutableRustupInit(pathValue string) bool {
	parts := strings.Split(strings.TrimPrefix(pathValue, "/"), "/")
	if len(parts) != 4 || parts[0] != "rustup" || parts[1] != "dist" || parts[2] == "" {
		return false
	}
	return rustupInitName(parts[3])
}

func isArchiveRustupInit(pathValue string) bool {
	parts := strings.Split(strings.TrimPrefix(pathValue, "/"), "/")
	if len(parts) != 5 || parts[0] != "rustup" || parts[1] != "archive" || parts[2] == "" || parts[3] == "" {
		return false
	}
	return rustupInitName(parts[4])
}

func rustupInitName(name string) bool {
	return name == "rustup-init" || name == "rustup-init.exe" ||
		name == "rustup-init.sha256" || name == "rustup-init.exe.sha256"
}

func checksumFor(key string) (artifactKey, algo string, ok bool) {
	if !strings.HasSuffix(key, ".sha256") {
		return "", "", false
	}
	return strings.TrimSuffix(key, ".sha256"), "sha256", true
}

type manifest struct {
	Pkg map[string]struct {
		Target map[string]struct {
			Hash   string `toml:"hash"`
			XzHash string `toml:"xz_hash"`
			URL    string `toml:"url"`
			XzURL  string `toml:"xz_url"`
		} `toml:"target"`
	} `toml:"pkg"`
}

func (k *Kind) ingest(_ context.Context, obj kinds.ObjectRef, raw io.Reader, sink kinds.Sink) error {
	data, err := io.ReadAll(io.LimitReader(raw, k.parseLimit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > k.parseLimit {
		return fmt.Errorf("rustup manifest exceeds parse limit")
	}
	var document manifest
	if err := toml.Unmarshal(data, &document); err != nil {
		sink.Logger().Warn("rustup_manifest_parse_failed", "repo", obj.Repo, "key", obj.Key, "error", err)
		return nil
	}
	for _, pkg := range document.Pkg {
		for _, target := range pkg.Target {
			k.expect(sink, target.URL, target.Hash)
			k.expect(sink, target.XzURL, target.XzHash)
		}
	}
	return nil
}

func (k *Kind) expect(sink kinds.Sink, rawURL, hash string) {
	if rawURL == "" || hash == "" {
		return
	}
	key, ok := k.artifactKey(rawURL)
	if !ok {
		return
	}
	sink.Expect(key, "sha256", hash)
}

func (k *Kind) artifactKey(rawURL string) (string, bool) {
	if strings.HasPrefix(rawURL, "/") {
		return rawURL, true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Path == "" {
		return "", false
	}
	pathValue := parsed.EscapedPath()
	for _, upstream := range k.upstreams {
		parsedUpstream, err := url.Parse(upstream)
		if err != nil || parsedUpstream.Path == "" || parsedUpstream.Path == "/" {
			continue
		}
		base := strings.TrimSuffix(parsedUpstream.EscapedPath(), "/") + "/"
		if strings.HasPrefix(pathValue, base) {
			pathValue = "/" + strings.TrimPrefix(pathValue, base)
			break
		}
	}
	if !strings.HasPrefix(pathValue, "/") {
		pathValue = "/" + pathValue
	}
	return pathValue, true
}

var _ kinds.Kind = (*Kind)(nil)
