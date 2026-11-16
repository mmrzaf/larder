// Package transfer exports, imports and ingests cached objects.
package transfer

import (
	"fmt"
	"path"
	"strings"
)

// ManifestName is the export manifest file name.
const ManifestName = "larder-export.json"

// DataVersion is the manifest format version.
const DataVersion = 1

// Manifest describes an exported tree or archive.
type Manifest struct {
	DataVersion int             `json:"data_version"`
	BaseURL     string          `json:"base_url,omitempty"`
	ExportedAt  string          `json:"exported_at"`
	Repos       []ManifestRepo  `json:"repos"`
	Objects     []ManifestEntry `json:"objects"`
}

// ManifestRepo identifies one exported repository.
type ManifestRepo struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Mount string `json:"mount"`
}

// ManifestEntry is one exported object.
type ManifestEntry struct {
	Repo        string `json:"repo"`
	Key         string `json:"key"`
	Path        string `json:"path"`
	Class       string `json:"class"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	Verified    bool   `json:"verified"`
	FetchedAt   string `json:"fetched_at,omitempty"`
	ValidatedAt string `json:"validated_at,omitempty"`
}

func validateRelative(value string) error {
	if value == "" {
		return fmt.Errorf("empty path")
	}
	if strings.ContainsAny(value, "\\:\x00") || strings.HasPrefix(value, "/") {
		return fmt.Errorf("unsafe path %q", value)
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("unsafe path %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "" {
			return fmt.Errorf("unsafe path %q", value)
		}
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || windowsReserved(part) {
			return fmt.Errorf("unsafe path %q", value)
		}
	}
	return nil
}

func objectPath(repo, key string) (string, error) {
	if repo == "" {
		return "", fmt.Errorf("empty repository")
	}
	if !strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("key %q is not absolute", key)
	}
	value := repo + key
	if err := validateRelative(value); err != nil {
		return "", err
	}
	return value, nil
}

func windowsReserved(name string) bool {
	base := strings.ToUpper(name)
	if index := strings.IndexByte(base, '.'); index >= 0 {
		base = base[:index]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}
