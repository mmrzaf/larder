// Package gokind implements the GOPROXY module and checksum database protocol.
package gokind

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
)

// Kind classifies Go module and sumdb requests.
type Kind struct {
	opts config.GoOptions
}

// New builds the Go kind.
func New(repo config.Repo) (kinds.Kind, error) {
	if repo.Go == nil {
		return nil, fmt.Errorf("go repo %s has no go options", repo.Name)
	}
	return &Kind{opts: *repo.Go}, nil
}

// Name returns the kind name.
func (k *Kind) Name() string { return "go" }

// Classify maps a Go proxy path to a plan.
func (k *Kind) Classify(req kinds.Request) (kinds.Plan, error) {
	pathValue := req.Path
	if strings.HasPrefix(pathValue, "/sumdb/") {
		return k.classifySumdb(pathValue)
	}
	trimmed := strings.TrimPrefix(pathValue, "/")
	if trimmed == "" {
		return kinds.Plan{}, kinds.Status(400, "invalid module path")
	}
	plan := kinds.Plan{Key: pathValue, UpstreamPath: pathValue}
	switch {
	case strings.HasSuffix(trimmed, "/@v/list"):
		escaped := strings.TrimSuffix(trimmed, "/@v/list")
		if err := checkModule(escaped); err != nil {
			return kinds.Plan{}, kinds.Status(400, err.Error())
		}
		plan.Class = kinds.Mutable
		plan.FreshTTL = 60 * time.Second
		plan.Binary = true
		return plan, nil
	case strings.HasSuffix(trimmed, "/@latest"):
		escaped := strings.TrimSuffix(trimmed, "/@latest")
		if err := checkModule(escaped); err != nil {
			return kinds.Plan{}, kinds.Status(400, err.Error())
		}
		plan.Class = kinds.Mutable
		plan.FreshTTL = 60 * time.Second
		plan.Binary = true
		return plan, nil
	}

	index := strings.LastIndex(trimmed, "/@v/")
	if index < 0 {
		return kinds.Plan{Class: kinds.Deny, Key: pathValue, UpstreamPath: pathValue}, nil
	}
	escapedModule := trimmed[:index]
	file := trimmed[index+4:]
	version, ext, ok := splitArtifact(file)
	if !ok {
		return kinds.Plan{Class: kinds.Deny, Key: pathValue, UpstreamPath: pathValue}, nil
	}
	if err := checkModule(escapedModule); err != nil {
		return kinds.Plan{}, kinds.Status(400, err.Error())
	}
	unescapedVersion, err := module.UnescapeVersion(version)
	if err != nil {
		return kinds.Plan{}, kinds.Status(400, "invalid module version")
	}

	switch ext {
	case ".info":
		plan.Binary = true
		plan.Hooks.Validate = infoValidator
		if semver.IsValid(unescapedVersion) {
			plan.Class = kinds.Immutable
		} else {
			plan.Class = kinds.Mutable
			plan.FreshTTL = 60 * time.Second
		}
	case ".mod":
		if !semver.IsValid(unescapedVersion) {
			return kinds.Plan{Class: kinds.Deny, Key: pathValue, UpstreamPath: pathValue}, nil
		}
		plan.Class = kinds.Immutable
		plan.Binary = true
		plan.Hooks.Validate = modValidator
		if k.opts.Verify == "strict" {
			plan.Hooks.Verify = strictVerifier(escapedModule, version, unescapedVersion, false)
		}
	case ".zip":
		if !semver.IsValid(unescapedVersion) {
			return kinds.Plan{Class: kinds.Deny, Key: pathValue, UpstreamPath: pathValue}, nil
		}
		plan.Class = kinds.Immutable
		plan.Binary = true
		prefix := escapedModule + "@" + version + "/"
		plan.Hooks.Validate = zipValidator(prefix)
		if k.opts.Verify == "strict" {
			plan.Hooks.Verify = strictVerifier(escapedModule, version, unescapedVersion, true)
		}
	default:
		return kinds.Plan{Class: kinds.Deny, Key: pathValue, UpstreamPath: pathValue}, nil
	}
	return plan, nil
}

func (k *Kind) classifySumdb(pathValue string) (kinds.Plan, error) {
	rest := strings.TrimPrefix(pathValue, "/sumdb/")
	name, suffix, ok := strings.Cut(rest, "/")
	if !ok || name == "" {
		return kinds.Plan{}, kinds.Status(404, "unknown sumdb path")
	}
	configured := k.opts.SumDBUpstreams[name]
	if len(configured) == 0 && name != "sum.golang.org" {
		return kinds.Plan{Class: kinds.Deny, Key: pathValue, UpstreamPath: pathValue}, nil
	}
	plan := kinds.Plan{Key: pathValue, UpstreamPath: pathValue}
	switch {
	case suffix == "supported":
		plan.Class = kinds.Mutable
		plan.FreshTTL = time.Hour
	case suffix == "latest":
		plan.Class = kinds.Mutable
		plan.FreshTTL = 60 * time.Second
	case strings.HasPrefix(suffix, "lookup/"):
		plan.Class = kinds.Immutable
		plan.Binary = true
	case strings.HasPrefix(suffix, "tile/"):
		plan.Class = kinds.Immutable
		plan.Binary = true
	default:
		return kinds.Plan{Class: kinds.Deny, Key: pathValue, UpstreamPath: pathValue}, nil
	}
	plan.Hooks.Resolve = func(ups []kinds.Upstream) ([]kinds.URLCandidate, error) {
		var candidates []kinds.URLCandidate
		if len(configured) > 0 {
			for _, base := range configured {
				candidates = append(candidates, kinds.URLCandidate{URL: strings.TrimSuffix(base, "/") + "/" + suffix})
			}
		} else {
			for _, up := range ups {
				candidates = append(candidates, kinds.URLCandidate{URL: strings.TrimSuffix(up.URL, "/") + pathValue})
			}
			candidates = append(candidates, kinds.URLCandidate{URL: "https://sum.golang.org/" + suffix})
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no sumdb upstreams for %s", name)
		}
		return candidates, nil
	}
	return plan, nil
}

func checkModule(escaped string) error {
	if escaped == "" {
		return fmt.Errorf("empty module path")
	}
	unescaped, err := module.UnescapePath(escaped)
	if err != nil {
		return fmt.Errorf("invalid module path")
	}
	if err := module.CheckPath(unescaped); err != nil {
		return fmt.Errorf("invalid module path")
	}
	return nil
}

func splitArtifact(file string) (version, ext string, ok bool) {
	for _, candidate := range []string{".info", ".mod", ".zip"} {
		if strings.HasSuffix(file, candidate) {
			version = strings.TrimSuffix(file, candidate)
			if version == "" {
				return "", "", false
			}
			return version, candidate, true
		}
	}
	return "", "", false
}

func infoValidator(_ context.Context, file *os.File, size int64) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return err
	}
	var info struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(data, &info); err != nil || info.Version == "" {
		return fmt.Errorf("invalid .info document")
	}
	return nil
}

func modValidator(_ context.Context, file *os.File, size int64) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, 16<<20))
	if err != nil {
		return err
	}
	if len(data) == 0 || !utf8.Valid(data) {
		return fmt.Errorf("invalid .mod document")
	}
	return nil
}

func zipValidator(prefix string) func(context.Context, *os.File, int64) error {
	return func(_ context.Context, file *os.File, size int64) error {
		reader, err := zip.NewReader(file, size)
		if err != nil {
			return fmt.Errorf("invalid module zip: %w", err)
		}
		for _, entry := range reader.File {
			if !strings.HasPrefix(entry.Name, prefix) {
				return fmt.Errorf("zip entry %q is outside %s", entry.Name, prefix)
			}
		}
		return nil
	}
}

func strictVerifier(escapedModule, escapedVersion, unescapedVersion string, isZip bool) func(context.Context, io.ReaderAt, int64, kinds.FetchFunc) (string, error) {
	unescapedModule, err := module.UnescapePath(escapedModule)
	if err != nil {
		unescapedModule = escapedModule
	}
	lookup := "/sumdb/sum.golang.org/lookup/" + escapedModule + "@" + escapedVersion
	return func(ctx context.Context, raw io.ReaderAt, size int64, fetch kinds.FetchFunc) (string, error) {
		body, err := fetch(ctx, lookup)
		if err != nil {
			return "", fmt.Errorf("%w: %v", kinds.ErrVerificationUnavailable, err)
		}
		want, err := lookupHash(body, unescapedModule, unescapedVersion, isZip)
		if err != nil {
			return "", fmt.Errorf("%w: %v", kinds.ErrVerificationUnavailable, err)
		}
		got, err := computeHash(raw, size, isZip)
		if err != nil {
			return "", err
		}
		if got != want {
			return "", fmt.Errorf("dirhash mismatch for %s@%s: expected %s, got %s", unescapedModule, unescapedVersion, want, got)
		}
		return got, nil
	}
}

func lookupHash(body []byte, modulePath, version string, isZip bool) (string, error) {
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		if fields[0] != modulePath {
			continue
		}
		if isZip {
			if fields[1] != version {
				continue
			}
		} else if fields[1] != version+"/go.mod" {
			continue
		}
		hash, ok := strings.CutPrefix(fields[2], "h1:")
		if !ok || hash == "" {
			return "", fmt.Errorf("missing h1 hash")
		}
		return hash, nil
	}
	return "", fmt.Errorf("no h1 line for %s@%s", modulePath, version)
}

func computeHash(raw io.ReaderAt, size int64, isZip bool) (string, error) {
	if isZip {
		reader, err := zip.NewReader(raw, size)
		if err != nil {
			return "", fmt.Errorf("invalid module zip: %w", err)
		}
		names := make([]string, 0, len(reader.File))
		entries := make(map[string]*zip.File, len(reader.File))
		for _, entry := range reader.File {
			names = append(names, entry.Name)
			entries[entry.Name] = entry
		}
		sort.Strings(names)
		hash, err := dirhash.Hash1(names, func(name string) (io.ReadCloser, error) {
			entry := entries[name]
			if entry == nil {
				return nil, fmt.Errorf("zip entry %q is missing", name)
			}
			return entry.Open()
		})
		if err != nil {
			return "", err
		}
		return strings.TrimPrefix(hash, "h1:"), nil
	}
	if size > 16<<20 {
		return "", fmt.Errorf("go.mod exceeds the hashing limit")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(raw, 0, size), data); err != nil {
		return "", err
	}
	hash, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(hash, "h1:"), nil
}

var _ kinds.Kind = (*Kind)(nil)
