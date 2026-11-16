// Package cratekind implements the crates.io sparse index and crate downloads.
package cratekind

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/kinds"
)

// Kind classifies crates index and download paths.
type Kind struct {
	mount       string
	dlUpstreams []string
}

// New builds a crates kind from repository configuration.
func New(repo config.Repo) (kinds.Kind, error) {
	var templates []string
	if repo.Crates != nil {
		templates = append(templates, repo.Crates.DLUpstreams...)
	}
	if len(templates) == 0 {
		templates = []string{"https://static.crates.io/crates/{crate}/{crate}-{version}.crate"}
	}
	mount := repo.Mount
	if mount == "" {
		mount = "/" + repo.Name
	}
	return &Kind{mount: mount, dlUpstreams: templates}, nil
}

// Name returns the kind name.
func (k *Kind) Name() string { return "crates" }

// Classify maps a crates path to a plan.
func (k *Kind) Classify(req kinds.Request) (kinds.Plan, error) {
	pathValue := req.Path
	plan := kinds.Plan{Key: pathValue, UpstreamPath: pathValue}
	switch {
	case pathValue == "/index/config.json":
		base := strings.TrimSuffix(req.PublicBase, "/")
		mount := ""
		if k.mount != "" && k.mount != "/" {
			mount = "/" + strings.Trim(k.mount, "/")
		}
		dl := base + mount + "/dl"
		plan.Class = kinds.Local
		plan.Local = &kinds.LocalResponse{
			Status:      http.StatusOK,
			Body:        []byte(fmt.Sprintf(`{"dl":%q}`, dl)),
			ContentType: "application/json",
		}
		return plan, nil
	case strings.HasPrefix(pathValue, "/index/"):
		relative := strings.TrimPrefix(pathValue, "/index")
		if !validIndexPath(relative) {
			plan.Class = kinds.Deny
			return plan, nil
		}
		plan.Class = kinds.Mutable
		plan.UpstreamPath = relative
		plan.Hooks.Ingest = k.ingest
		return plan, nil
	case strings.HasPrefix(pathValue, "/dl/"):
		crate, version, ok := downloadPath(pathValue)
		if !ok {
			plan.Class = kinds.Deny
			return plan, nil
		}
		plan.Class = kinds.Immutable
		plan.Binary = true
		plan.Hooks.Resolve = k.resolveDownload(crate, version)
		return plan, nil
	default:
		plan.Class = kinds.Deny
		return plan, nil
	}
}

func validIndexPath(relative string) bool {
	if !strings.HasPrefix(relative, "/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(relative, "/"), "/")
	switch len(parts) {
	case 2:
		if parts[0] == "1" {
			return validName(parts[1]) && len(parts[1]) == 1
		}
		if parts[0] == "2" {
			return validName(parts[1]) && len(parts[1]) == 2
		}
		return false
	case 3:
		if parts[0] == "3" {
			if !validName(parts[1]) || len(parts[1]) != 1 {
				return false
			}
			return validName(parts[2]) && len(parts[2]) == 3 && strings.HasPrefix(parts[2], parts[1])
		}
		if !validName(parts[0]) || len(parts[0]) != 2 || !validName(parts[1]) || len(parts[1]) != 2 {
			return false
		}
		return validName(parts[2]) && len(parts[2]) >= 4 && strings.HasPrefix(parts[2], parts[0]+parts[1])
	default:
		return false
	}
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func downloadPath(pathValue string) (crate, version string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(pathValue, "/dl/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] != "download" {
		return "", "", false
	}
	if !validName(parts[0]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (k *Kind) resolveDownload(crate, version string) func([]kinds.Upstream) ([]kinds.URLCandidate, error) {
	return func([]kinds.Upstream) ([]kinds.URLCandidate, error) {
		candidates := make([]kinds.URLCandidate, 0, len(k.dlUpstreams))
		for _, template := range k.dlUpstreams {
			url := strings.ReplaceAll(template, "{crate}", crate)
			url = strings.ReplaceAll(url, "{version}", version)
			candidates = append(candidates, kinds.URLCandidate{URL: url})
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no download upstreams for crate %s", crate)
		}
		return candidates, nil
	}
}

func (k *Kind) ingest(_ context.Context, obj kinds.ObjectRef, raw io.Reader, sink kinds.Sink) error {
	scanner := bufio.NewScanner(raw)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var entry struct {
			Name  string `json:"name"`
			Vers  string `json:"vers"`
			Cksum string `json:"cksum"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			sink.Logger().Debug("crates_index_line_invalid", "repo", obj.Repo, "key", obj.Key)
			continue
		}
		if entry.Name == "" || entry.Vers == "" || entry.Cksum == "" {
			continue
		}
		sink.Expect("/dl/"+entry.Name+"/"+entry.Vers+"/download", "sha256", entry.Cksum)
	}
	if err := scanner.Err(); err != nil {
		sink.Logger().Warn("crates_index_scan_failed", "repo", obj.Repo, "key", obj.Key, "error", err)
	}
	return nil
}

var _ kinds.Kind = (*Kind)(nil)
