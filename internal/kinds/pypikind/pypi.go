// Package pypikind implements the PyPI simple repository API and file downloads.
package pypikind

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/rewrite"
)

var fileSuffixes = []string{".tar.gz", ".tar.bz2", ".whl", ".zip", ".egg", ".exe", ".msi", ".metadata", ".asc"}

// Kind classifies PyPI index, metadata and file paths.
type Kind struct {
	mount      string
	filesHosts map[string]bool
	schemes    map[string]string
	hosts      []string
}

// New builds a PyPI kind from repository configuration.
func New(repo config.Repo) (kinds.Kind, error) {
	hosts := make(map[string]bool)
	schemes := make(map[string]string)
	for _, upstream := range repo.Upstreams {
		parsed, err := url.Parse(upstream.URL)
		if err != nil || parsed.Host == "" {
			continue
		}
		hosts[strings.ToLower(parsed.Host)] = true
		schemes[strings.ToLower(parsed.Host)] = parsed.Scheme
	}
	filesHosts := []string{"files.pythonhosted.org"}
	if repo.PyPI != nil && len(repo.PyPI.FilesHosts) > 0 {
		filesHosts = repo.PyPI.FilesHosts
	}
	for _, host := range filesHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			continue
		}
		hosts[host] = true
		if schemes[host] == "" {
			schemes[host] = "https"
		}
	}
	mount := repo.Mount
	if mount == "" {
		mount = "/" + repo.Name
	}
	hostList := make([]string, 0, len(hosts))
	for host := range hosts {
		hostList = append(hostList, host)
	}
	return &Kind{mount: mount, filesHosts: hosts, schemes: schemes, hosts: hostList}, nil
}

// Name returns the kind name.
func (k *Kind) Name() string { return "pypi" }

// Classify maps a PyPI request path to a plan.
func (k *Kind) Classify(req kinds.Request) (kinds.Plan, error) {
	pathValue := req.Path
	upstreamPath := withQuery(pathValue, req.RawQuery)
	plan := kinds.Plan{Key: pathValue, UpstreamPath: upstreamPath}
	switch {
	case pathValue == "/simple":
		plan.Redirect = k.mountPath("/simple/")
		return plan, nil
	case pathValue == "/simple/":
		plan.Class = kinds.Mutable
		plan.FreshTTL = 3600e9
		k.indexPlan(&plan, req)
		return plan, nil
	case strings.HasPrefix(pathValue, "/simple/"):
		rest := strings.TrimPrefix(pathValue, "/simple/")
		project := strings.TrimSuffix(rest, "/")
		if project == "" || strings.Contains(project, "/") {
			plan.Class = kinds.Deny
			return plan, nil
		}
		normalized := normalizeProject(project)
		if normalized != project || !strings.HasSuffix(rest, "/") {
			plan.Redirect = k.mountPath("/simple/" + normalized + "/")
			return plan, nil
		}
		plan.Class = kinds.Mutable
		plan.Key = "/simple/" + normalized + "/"
		plan.UpstreamPath = plan.Key
		k.indexPlan(&plan, req)
		return plan, nil
	case strings.HasPrefix(pathValue, "/_files/"):
		host, rest, ok := splitFiles(pathValue)
		if !ok || !k.filesHosts[host] {
			return kinds.Plan{}, kinds.Status(http.StatusForbidden, "file host is not allowed")
		}
		plan.Class = kinds.Immutable
		plan.Binary = true
		plan.Hooks.Resolve = func([]kinds.Upstream) ([]kinds.URLCandidate, error) {
			scheme := k.schemes[host]
			if scheme == "" {
				scheme = "https"
			}
			return []kinds.URLCandidate{{URL: scheme + "://" + host + "/" + rest, RestrictHosts: true}}, nil
		}
		return plan, nil
	case jsonAPI(pathValue):
		plan.Class = kinds.Mutable
		plan.Hooks.Rewrite = k.rewriteDocument
		return plan, nil
	case relativeFile(pathValue):
		plan.Class = kinds.Immutable
		plan.Binary = true
		return plan, nil
	default:
		plan.Class = kinds.Deny
		return plan, nil
	}
}

func (k *Kind) indexPlan(plan *kinds.Plan, req kinds.Request) {
	accept := req.Header.Get("Accept")
	if accept != "" {
		plan.UpstreamHdr = http.Header{"Accept": []string{accept}}
	}
	if isJSONAccept(accept) {
		plan.Variant = "json"
	} else {
		plan.Variant = "html"
	}
	plan.Hooks.Rewrite = k.rewriteDocument
	plan.Hooks.Ingest = k.ingest
}

func (k *Kind) mountPath(value string) string {
	if k.mount == "" || k.mount == "/" {
		return value
	}
	return "/" + strings.Trim(k.mount, "/") + value
}

func isJSONAccept(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "application/vnd.pypi.simple.v1+json") ||
		strings.Contains(value, "application/vnd.pypi.simple.latest+json")
}

func normalizeProject(value string) string {
	value = strings.ToLower(value)
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		if r == '-' || r == '_' || r == '.' {
			if !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
			continue
		}
		builder.WriteRune(r)
		lastDash = false
	}
	return builder.String()
}

func splitFiles(pathValue string) (string, string, bool) {
	rest := strings.TrimPrefix(pathValue, "/_files/")
	host, remainder, ok := strings.Cut(rest, "/")
	if !ok || host == "" || remainder == "" {
		return "", "", false
	}
	if decoded, err := url.PathUnescape(host); err == nil {
		host = decoded
	}
	return strings.ToLower(host), remainder, true
}

func jsonAPI(pathValue string) bool {
	trimmed := strings.TrimPrefix(pathValue, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] == "json" {
		return true
	}
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] == "json"
}

func relativeFile(pathValue string) bool {
	lower := strings.ToLower(pathValue)
	for _, suffix := range fileSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func withQuery(pathValue, rawQuery string) string {
	if rawQuery == "" {
		return pathValue
	}
	return pathValue + "?" + rawQuery
}

func (k *Kind) rewriteDocument(raw []byte, publicBase string) ([]byte, error) {
	base := strings.TrimSuffix(publicBase, "/") + k.mountPath("") + "/_files/"
	replacements := make([]rewrite.Replacement, 0, len(k.hosts)*2)
	for _, host := range k.hosts {
		replacements = append(replacements,
			rewrite.Replacement{Prefix: "https://" + host + "/", Base: base + host + "/"},
			rewrite.Replacement{Prefix: "http://" + host + "/", Base: base + host + "/"},
		)
	}
	replacer, err := rewrite.New(replacements)
	if err != nil {
		return nil, err
	}
	return replacer.Buffer(raw), nil
}

func (k *Kind) fileKey(indexKey, reference string) (string, bool) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Path == "" {
		return "", false
	}
	if parsed.Host != "" {
		host := strings.ToLower(parsed.Host)
		if !k.filesHosts[host] {
			return "", false
		}
		return "/_files/" + host + parsed.EscapedPath(), true
	}
	base := &url.URL{Path: indexKey}
	resolved := base.ResolveReference(parsed)
	key := resolved.EscapedPath()
	if key == "" || !strings.HasPrefix(key, "/") {
		return "", false
	}
	return key, true
}

func fileDigest(reference string) (integrity.Digest, bool) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Fragment == "" {
		return integrity.Digest{}, false
	}
	fragment := parsed.Fragment
	if index := strings.IndexByte(fragment, '&'); index >= 0 {
		fragment = fragment[:index]
	}
	algo, value, ok := strings.Cut(fragment, "=")
	if !ok || !strings.EqualFold(algo, "sha256") {
		return integrity.Digest{}, false
	}
	digest, err := integrity.ParseHex("sha256", value)
	if err != nil {
		return integrity.Digest{}, false
	}
	return digest, true
}

func metadataDigest(value string) (integrity.Digest, bool) {
	value = strings.TrimSpace(value)
	algo, encoded, ok := strings.Cut(value, "=")
	if !ok || !strings.EqualFold(algo, "sha256") {
		return integrity.Digest{}, false
	}
	digest, err := integrity.ParseHex("sha256", encoded)
	if err != nil {
		return integrity.Digest{}, false
	}
	return digest, true
}

func (k *Kind) ingest(_ context.Context, obj kinds.ObjectRef, raw io.Reader, sink kinds.Sink) error {
	expectations := make(map[string]integrity.Digest)
	indexKey, _, _ := strings.Cut(obj.Key, "#")
	var digests []expected
	var err error
	if isJSONDocument(obj.ContentType, indexKey) {
		digests, err = parseJSONIndex(k, indexKey, raw)
	} else {
		digests, err = parseHTMLIndex(k, indexKey, raw)
	}
	if err != nil {
		sink.Logger().Warn("pypi_index_parse_failed", "repo", obj.Repo, "key", obj.Key, "error", err)
		return nil
	}
	for _, item := range digests {
		if item.digest.Hex != "" {
			expectations[item.key] = item.digest
		}
	}
	for key, digest := range expectations {
		sink.Expect(key, digest.Algo, digest.Hex)
	}
	return nil
}

func isJSONDocument(contentType, key string) bool {
	return strings.Contains(strings.ToLower(contentType), "json") || strings.HasSuffix(strings.ToLower(key), "/json")
}

type expected struct {
	key    string
	digest integrity.Digest
}

func parseHTMLIndex(k *Kind, indexKey string, raw io.Reader) ([]expected, error) {
	tokenizer := html.NewTokenizer(raw)
	var out []expected
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return out, nil
			}
			return nil, tokenizer.Err()
		case html.StartTagToken:
			token := tokenizer.Token()
			if token.Data != "a" {
				continue
			}
			var href, coreMetadata, distInfoMetadata string
			for _, attribute := range token.Attr {
				switch attribute.Key {
				case "href":
					href = attribute.Val
				case "data-core-metadata":
					coreMetadata = attribute.Val
				case "data-dist-info-metadata":
					distInfoMetadata = attribute.Val
				}
			}
			if href == "" {
				continue
			}
			key, ok := k.fileKey(indexKey, href)
			if !ok {
				continue
			}
			if digest, ok := fileDigest(href); ok {
				out = append(out, expected{key: key, digest: digest})
			}
			for _, value := range []string{coreMetadata, distInfoMetadata} {
				if digest, ok := metadataDigest(value); ok {
					out = append(out, expected{key: key + ".metadata", digest: digest})
				}
			}
		}
	}
}

func parseJSONIndex(k *Kind, indexKey string, raw io.Reader) ([]expected, error) {
	decoder := json.NewDecoder(raw)
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("index is not a JSON object")
	}
	var out []expected
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyToken.(string)
		if key != "files" {
			if err := skipValue(decoder); err != nil {
				return nil, err
			}
			continue
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := token.(json.Delim); !ok || delim != '[' {
			if delim == '{' {
				if err := skipValueAfterOpen(decoder, delim); err != nil {
					return nil, err
				}
			}
			continue
		}
		for decoder.More() {
			var file struct {
				URL              string            `json:"url"`
				Filename         string            `json:"filename"`
				Hashes           map[string]string `json:"hashes"`
				CoreMetadata     json.RawMessage   `json:"core-metadata"`
				DistInfoMetadata json.RawMessage   `json:"dist-info-metadata"`
			}
			if err := decoder.Decode(&file); err != nil {
				return nil, err
			}
			reference := file.URL
			if reference == "" {
				reference = file.Filename
			}
			fileKey, ok := k.fileKey(indexKey, reference)
			if !ok {
				continue
			}
			if value := file.Hashes["sha256"]; value != "" {
				if digest, err := integrity.ParseHex("sha256", value); err == nil {
					out = append(out, expected{key: fileKey, digest: digest})
				}
			}
			for _, rawMetadata := range []json.RawMessage{file.CoreMetadata, file.DistInfoMetadata} {
				if digest, ok := jsonMetadataDigest(rawMetadata); ok {
					out = append(out, expected{key: fileKey + ".metadata", digest: digest})
				}
			}
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func jsonMetadataDigest(raw json.RawMessage) (integrity.Digest, bool) {
	if len(raw) == 0 {
		return integrity.Digest{}, false
	}
	var object struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.SHA256 != "" {
		if digest, err := integrity.ParseHex("sha256", object.SHA256); err == nil {
			return digest, true
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return metadataDigest(text)
	}
	return integrity.Digest{}, false
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
