// Package server implements the public and administrative HTTP surface.
package server

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizePath validates and normalizes an escaped request path.
func NormalizePath(escaped string) (string, error) {
	if escaped == "" {
		return "/", nil
	}
	if !strings.HasPrefix(escaped, "/") {
		return "", fmt.Errorf("path must start with /")
	}
	if containsControl(escaped) || strings.ContainsRune(escaped, '\\') {
		return "", fmt.Errorf("path contains a forbidden character")
	}

	segments := strings.Split(escaped, "/")
	kept := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		if err := validateSegment(segment); err != nil {
			return "", err
		}
		kept = append(kept, segment)
	}
	normalized := "/" + strings.Join(kept, "/")
	if strings.HasSuffix(escaped, "/") && normalized != "/" {
		normalized += "/"
	}
	return normalized, nil
}

func validateSegment(segment string) error {
	current := segment
	for depth := 0; depth < 4; depth++ {
		if containsControl(current) || strings.ContainsRune(current, '\\') {
			return fmt.Errorf("path contains a forbidden character")
		}
		if dangerousSegment(current) {
			return fmt.Errorf("path contains a traversal segment")
		}
		next, err := url.PathUnescape(current)
		if err != nil {
			return fmt.Errorf("path contains invalid percent-encoding")
		}
		if next == current {
			return nil
		}
		current = next
	}
	return fmt.Errorf("path contains excessive encoding")
}

func dangerousSegment(segment string) bool {
	if segment == "." || segment == ".." {
		return true
	}
	return strings.HasPrefix(segment, "../") ||
		strings.HasSuffix(segment, "/..") ||
		strings.Contains(segment, "/../")
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// StripMount returns the path after mount, or false when the path is outside it.
func StripMount(pathValue, mount string) (string, bool) {
	if mount == "" {
		return pathValue, false
	}
	if mount == "/" {
		return pathValue, true
	}
	if pathValue == mount {
		return "/", true
	}
	if strings.HasPrefix(pathValue, mount+"/") {
		return pathValue[len(mount):], true
	}
	return "", false
}
