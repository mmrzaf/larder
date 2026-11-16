// Package aptkind implements Debian and Ubuntu archive repositories.
package aptkind

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/kinds"
)

const defaultParseLimit = 512 << 20

// Kind classifies apt archive paths and ingests signed metadata.
type Kind struct {
	suiteConsistency bool
	parseLimit       int64
}

// New builds an apt kind from repository configuration.
func New(repo config.Repo) (kinds.Kind, error) {
	consistency := true
	if repo.APT != nil {
		consistency = repo.APT.SuiteConsistency
	}
	limit := int64(defaultParseLimit)
	if repo.MaxParseBytes > 0 {
		limit = repo.MaxParseBytes.Bytes()
	}
	return &Kind{suiteConsistency: consistency, parseLimit: limit}, nil
}

// Name returns the kind name.
func (k *Kind) Name() string { return "apt" }

// Classify maps an apt archive path to a plan.
func (k *Kind) Classify(req kinds.Request) (kinds.Plan, error) {
	pathValue := req.Path
	plan := kinds.Plan{Key: pathValue, UpstreamPath: pathValue}
	switch {
	case strings.HasPrefix(pathValue, "/pool/"):
		plan.Class = kinds.Immutable
		plan.Binary = true
		return plan, nil
	case strings.HasPrefix(pathValue, "/dists/"):
		if digest, ok := byHashDigest(pathValue); ok {
			plan.Class = kinds.Immutable
			plan.Binary = true
			plan.Hooks.Expect = func(string) []integrity.Digest { return []integrity.Digest{digest} }
			return plan, nil
		}
		if malformedByHash(pathValue) {
			plan.Class = kinds.Deny
			return plan, nil
		}
		plan.Class = kinds.Mutable
		plan.FreshTTL = 300 * time.Second
		plan.Hooks.Ingest = k.ingest
		plan.Hooks.RefreshOnMismatch = refreshKeys
		return plan, nil
	case strings.HasPrefix(pathValue, "/project/"),
		strings.HasPrefix(pathValue, "/indices/"),
		strings.HasPrefix(pathValue, "/ls-lR"):
		plan.Class = kinds.Deny
		return plan, nil
	default:
		plan.Class = kinds.Deny
		return plan, nil
	}
}

func byHashDigest(pathValue string) (integrity.Digest, bool) {
	index := strings.Index(pathValue, "/by-hash/")
	if index < 0 {
		return integrity.Digest{}, false
	}
	rest := pathValue[index+len("/by-hash/"):]
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return integrity.Digest{}, false
	}
	algo := strings.ToLower(parts[0])
	switch algo {
	case "sha256", "sha512", "sha1", "md5":
	default:
		return integrity.Digest{}, false
	}
	digest, err := integrity.ParseHex(algo, parts[1])
	if err != nil {
		return integrity.Digest{}, false
	}
	return digest, true
}

func malformedByHash(pathValue string) bool {
	return strings.Contains(pathValue, "/by-hash/")
}

func (k *Kind) ingest(_ context.Context, obj kinds.ObjectRef, raw io.Reader, sink kinds.Sink) error {
	base := path.Base(obj.Key)
	switch {
	case base == "Release" || base == "InRelease":
		return k.ingestRelease(obj, raw, sink)
	case base == "Packages" || strings.HasPrefix(base, "Packages."):
		return k.ingestPackages(obj, raw, sink)
	default:
		return nil
	}
}

func (k *Kind) ingestRelease(obj kinds.ObjectRef, raw io.Reader, sink kinds.Sink) error {
	data, err := io.ReadAll(io.LimitReader(raw, k.parseLimit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > k.parseLimit {
		return fmt.Errorf("release file exceeds parse limit")
	}
	suite := suiteFor(obj.Key)
	if suite == "" {
		return nil
	}
	inSHA256 := false
	byHash := false
	for _, line := range releaseBody(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" {
			inSHA256 = false
			continue
		}
		if strings.HasPrefix(line, "Acquire-By-Hash:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "Acquire-By-Hash:"))
			byHash = strings.EqualFold(value, "yes")
			continue
		}
		if strings.HasPrefix(line, "SHA256:") {
			inSHA256 = true
			continue
		}
		if !inSHA256 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		digest, err := integrity.ParseHex("sha256", fields[0])
		if err != nil {
			continue
		}
		relative := fields[2]
		if strings.HasPrefix(relative, "/") {
			relative = strings.TrimPrefix(relative, "/")
		}
		replaceExpectation(sink, "/dists/"+suite+"/"+relative, "sha256", digest.Hex)
		if byHash {
			directory := path.Dir(relative)
			if directory != "." {
				sink.Expect("/dists/"+suite+"/"+directory+"/by-hash/SHA256/"+digest.Hex, "sha256", digest.Hex)
			}
		}
	}
	if k.suiteConsistency {
		sink.MarkStale("/dists/" + suite + "/**")
	}
	return nil
}

type expectationReplacer interface {
	ReplaceExpectation(key, algo, digest string)
}

func replaceExpectation(sink kinds.Sink, key, algo, digest string) {
	if replacer, ok := sink.(expectationReplacer); ok {
		replacer.ReplaceExpectation(key, algo, digest)
		return
	}
	sink.Expect(key, algo, digest)
}

func releaseBody(data string) []string {
	lines := strings.Split(data, "\n")
	start := 0
	if len(lines) > 0 && strings.HasPrefix(lines[0], "-----BEGIN PGP SIGNED MESSAGE-----") {
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				start = i + 1
				break
			}
		}
	}
	out := make([]string, 0, len(lines)-start)
	for _, line := range lines[start:] {
		if strings.HasPrefix(line, "- ") {
			line = line[2:]
		}
		out = append(out, line)
	}
	return out
}

func (k *Kind) ingestPackages(obj kinds.ObjectRef, raw io.Reader, sink kinds.Sink) error {
	reader, err := decompress(obj.Key, raw)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	limited := &limitReader{reader: reader, remaining: k.parseLimit}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var filename, sha256 string
	flush := func() {
		if filename != "" && sha256 != "" {
			if digest, err := integrity.ParseHex("sha256", sha256); err == nil {
				sink.Expect("/"+strings.TrimPrefix(filename, "/"), "sha256", digest.Hex)
			}
		}
		filename, sha256 = "", ""
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "Filename:"):
			filename = strings.TrimSpace(strings.TrimPrefix(line, "Filename:"))
		case strings.HasPrefix(line, "SHA256:"):
			sha256 = strings.TrimSpace(strings.TrimPrefix(line, "SHA256:"))
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		if err == errParseLimit {
			return fmt.Errorf("packages file exceeds parse limit")
		}
		return err
	}
	if limited.remaining < 0 {
		return fmt.Errorf("packages file exceeds parse limit")
	}
	return nil
}

func decompress(key string, raw io.Reader) (io.ReadCloser, error) {
	switch {
	case strings.HasSuffix(key, ".gz"):
		return gzip.NewReader(raw)
	case strings.HasSuffix(key, ".xz"):
		reader, err := xz.NewReader(raw)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(reader), nil
	case strings.HasSuffix(key, ".bz2"):
		return io.NopCloser(bzip2.NewReader(raw)), nil
	default:
		return io.NopCloser(raw), nil
	}
}

type limitReader struct {
	reader    io.Reader
	remaining int64
}

func (r *limitReader) Read(p []byte) (int, error) {
	if r.remaining < 0 {
		return 0, errParseLimit
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return n, errParseLimit
	}
	return n, err
}

var errParseLimit = fmt.Errorf("parse limit exceeded")

func refreshKeys(key string) []string {
	suite := suiteFor(key)
	if suite == "" {
		return nil
	}
	return []string{"/dists/" + suite + "/Release", "/dists/" + suite + "/InRelease"}
}

func suiteFor(key string) string {
	if !strings.HasPrefix(key, "/dists/") {
		return ""
	}
	rest := strings.TrimPrefix(key, "/dists/")
	suite, _, _ := strings.Cut(rest, "/")
	return suite
}

var _ kinds.Kind = (*Kind)(nil)
