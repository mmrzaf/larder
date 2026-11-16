// Package npmkind implements the npm registry protocol for npm, pnpm, yarn and bun.
package npmkind

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/rewrite"
)

const installV1 = "application/vnd.npm.install-v1+json"

var registryPrefixes = []string{
	"https://registry.npmjs.org/",
	"http://registry.npmjs.org/",
	"https://registry.npmjs.com/",
	"https://registry.yarnpkg.com/",
}

// Kind classifies npm registry paths and rewrites packument tarball URLs.
type Kind struct {
	opts     config.NPMOptions
	mount    string
	prefixes []string
}

type dist struct {
	Tarball   string `json:"tarball"`
	Integrity string `json:"integrity"`
	Shasum    string `json:"shasum"`
}

// New builds an npm kind from repository configuration.
func New(repo config.Repo) (kinds.Kind, error) {
	if repo.NPM == nil {
		return nil, fmt.Errorf("npm repo %s has no npm options", repo.Name)
	}
	prefixes := make([]string, 0, len(repo.Upstreams)+len(registryPrefixes))
	for _, upstream := range repo.Upstreams {
		prefixes = append(prefixes, normalizePrefix(upstream.URL))
	}
	prefixes = append(prefixes, registryPrefixes...)
	mount := repo.Mount
	if mount == "" {
		mount = "/" + repo.Name
	}
	return &Kind{opts: *repo.NPM, mount: mount, prefixes: prefixes}, nil
}

func normalizePrefix(value string) string {
	return strings.TrimSuffix(value, "/") + "/"
}

func normalizeMount(mount string) string {
	if mount == "" || mount == "/" {
		return ""
	}
	return "/" + strings.Trim(mount, "/")
}

// Name returns the kind name.
func (k *Kind) Name() string { return "npm" }

// Classify maps an npm registry path to a plan.
func (k *Kind) Classify(req kinds.Request) (kinds.Plan, error) {
	pathValue := req.Path
	upstreamPath := withQuery(pathValue, req.RawQuery)
	plan := kinds.Plan{Key: pathValue, UpstreamPath: upstreamPath}
	switch {
	case strings.HasSuffix(strings.ToLower(pathValue), ".tgz"):
		plan.Class = kinds.Immutable
		plan.Binary = true
		return plan, nil
	case pathValue == "/-/ping":
		plan.Class = kinds.Local
		plan.Local = &kinds.LocalResponse{Status: http.StatusOK, Body: []byte("{}"), ContentType: "application/json"}
		return plan, nil
	case auditPath(pathValue):
		return k.auditPlan(plan), nil
	case pathValue == "/-/v1/search":
		plan.Class = kinds.Passthrough
		plan.FailureStatus = http.StatusBadGateway
		return plan, nil
	case strings.HasPrefix(pathValue, "/-/"):
		plan.Class = kinds.Mutable
		return plan, nil
	}

	scoped, normalized, versionDoc, ok := packagePath(pathValue)
	if !ok {
		plan.Class = kinds.Deny
		return plan, nil
	}
	plan.Class = kinds.Mutable
	plan.Key = normalized
	if versionDoc {
		return plan, nil
	}
	accept := req.Header.Get("Accept")
	if accept != "" {
		plan.UpstreamHdr = http.Header{"Accept": []string{accept}}
	}
	if strings.Contains(strings.ToLower(accept), installV1) {
		plan.Variant = "abbrev"
	} else {
		plan.Variant = "full"
	}
	plan.Hooks.Rewrite = k.rewriteDocument
	plan.Hooks.Ingest = k.ingest
	_ = scoped
	return plan, nil
}

func (k *Kind) auditPlan(plan kinds.Plan) kinds.Plan {
	plan.AllowPost = true
	plan.FailureStatus = http.StatusServiceUnavailable
	switch k.opts.Audit {
	case "stub":
		plan.Class = kinds.Local
		plan.Local = &kinds.LocalResponse{Status: http.StatusOK, Body: []byte("{}"), ContentType: "application/json"}
	case "deny":
		plan.Class = kinds.Deny
	default:
		plan.Class = kinds.Passthrough
		plan.Timeout = k.opts.AuditTimeout.Duration()
		plan.UpstreamHdr = http.Header{"Content-Type": []string{"application/json"}}
	}
	return plan
}

func auditPath(pathValue string) bool {
	for _, prefix := range []string{
		"/-/npm/v1/security/audits",
		"/-/npm/v1/security/advisories/bulk",
	} {
		if pathValue == prefix || strings.HasPrefix(pathValue, prefix+"/") {
			return true
		}
	}
	return false
}

func packagePath(pathValue string) (scoped bool, normalized string, versionDoc, ok bool) {
	if pathValue == "" || pathValue == "/" {
		return false, "", false, false
	}
	trimmed := strings.TrimPrefix(pathValue, "/")
	if strings.HasPrefix(trimmed, "@") {
		if index := strings.Index(trimmed, "/"); index >= 0 {
			scope, rest := trimmed[:index], trimmed[index+1:]
			if decoded, err := url.PathUnescape(trimmed); err == nil {
				trimmed = decoded
			}
			parts := strings.Split(trimmed, "/")
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				return true, "/" + trimmed, false, true
			}
			if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
				return true, "/" + trimmed, true, true
			}
			_ = scope
			_ = rest
			return false, "", false, false
		}
		if decoded, err := url.PathUnescape(trimmed); err == nil {
			trimmed = decoded
		}
		parts := strings.Split(trimmed, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return true, "/" + trimmed, false, true
		}
		return false, "", false, false
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 1 && parts[0] != "" {
		return false, "/" + parts[0], false, true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return false, "/" + trimmed, true, true
	}
	return false, "", false, false
}

func withQuery(pathValue, rawQuery string) string {
	if rawQuery == "" {
		return pathValue
	}
	return pathValue + "?" + rawQuery
}

func (k *Kind) rewriteDocument(raw []byte, publicBase string) ([]byte, error) {
	base := strings.TrimSuffix(publicBase, "/") + normalizeMount(k.mount) + "/"
	replacements := make([]rewrite.Replacement, 0, len(k.prefixes))
	for _, prefix := range k.prefixes {
		replacements = append(replacements, rewrite.Replacement{Prefix: prefix, Base: base})
	}
	replacer, err := rewrite.New(replacements)
	if err != nil {
		return nil, err
	}
	return replacer.JSONField(raw, "tarball")
}

func (k *Kind) ingest(_ context.Context, obj kinds.ObjectRef, raw io.Reader, sink kinds.Sink) error {
	expectations := make(map[string]integrity.Digest)
	err := parsePackument(raw, func(d dist) {
		key, ok := k.tarballKey(d.Tarball)
		if !ok {
			return
		}
		digest, ok := preferredDigest(d.Integrity, d.Shasum)
		if !ok {
			return
		}
		expectations[key] = digest
	})
	if err != nil {
		sink.Logger().Warn("npm_packument_parse_failed", "repo", obj.Repo, "key", obj.Key, "error", err)
		return nil
	}
	for key, digest := range expectations {
		sink.Expect(key, digest.Algo, digest.Hex)
	}
	return nil
}

func (k *Kind) tarballKey(tarball string) (string, bool) {
	if tarball == "" {
		return "", false
	}
	if strings.HasPrefix(tarball, "/") {
		return stripURLSuffix(tarball), true
	}
	for _, prefix := range k.prefixes {
		if !strings.HasPrefix(tarball, prefix) {
			continue
		}
		rest := strings.TrimPrefix(tarball, prefix)
		return "/" + stripURLSuffix(rest), true
	}
	return "", false
}

func stripURLSuffix(value string) string {
	if index := strings.IndexAny(value, "?#"); index >= 0 {
		return value[:index]
	}
	return value
}

func preferredDigest(integrityValue, shasum string) (integrity.Digest, bool) {
	if integrityValue != "" {
		digests, err := integrity.ParseSRI(integrityValue)
		if err == nil {
			byAlgo := make(map[string]integrity.Digest, len(digests))
			for _, digest := range digests {
				byAlgo[digest.Algo] = digest
			}
			for _, algo := range []string{"sha512", "sha256", "sha1"} {
				if digest, ok := byAlgo[algo]; ok {
					return digest, true
				}
			}
		}
	}
	if shasum != "" {
		if digest, err := integrity.ParseHex("sha1", shasum); err == nil {
			return digest, true
		}
	}
	return integrity.Digest{}, false
}

func parsePackument(raw io.Reader, emit func(dist)) error {
	decoder := json.NewDecoder(raw)
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("packument is not a JSON object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("packument key is not a string")
		}
		if key != "versions" {
			if err := skipValue(decoder); err != nil {
				return err
			}
			continue
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok || delim != '{' {
			if delim == '[' {
				if err := skipValueAfterOpen(decoder, delim); err != nil {
					return err
				}
			}
			continue
		}
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			var version struct {
				Dist dist `json:"dist"`
			}
			if err := decoder.Decode(&version); err != nil {
				return err
			}
			emit(version.Dist)
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
	}
	return nil
}

func skipValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	return skipValueAfterOpen(decoder, delim)
}

func skipValueAfterOpen(decoder *json.Decoder, opening json.Delim) error {
	if opening != '{' && opening != '[' {
		return nil
	}
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch delim {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return nil
}

var _ kinds.Kind = (*Kind)(nil)
