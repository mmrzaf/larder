// Package rewrite provides deterministic prefix rewriting for cached documents.
package rewrite

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Replacement maps one absolute prefix to another.
type Replacement struct {
	Prefix string
	Base   string
}

// Replacer applies a set of prefix replacements.
type Replacer struct {
	replacements []Replacement
	maxPrefix    int
}

// New builds a replacer, rejecting empty prefixes and sorting by length.
func New(replacements []Replacement) (*Replacer, error) {
	seen := make(map[string]bool, len(replacements))
	out := make([]Replacement, 0, len(replacements))
	for _, replacement := range replacements {
		if replacement.Prefix == "" {
			return nil, fmt.Errorf("rewrite prefix is empty")
		}
		if seen[replacement.Prefix] {
			continue
		}
		seen[replacement.Prefix] = true
		out = append(out, replacement)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Prefix) > len(out[j].Prefix) })
	maxPrefix := 0
	for _, replacement := range out {
		if len(replacement.Prefix) > maxPrefix {
			maxPrefix = len(replacement.Prefix)
		}
	}
	return &Replacer{replacements: out, maxPrefix: maxPrefix}, nil
}

// Buffer rewrites every prefix in data.
func (r *Replacer) Buffer(data []byte) []byte {
	if r == nil {
		return data
	}
	out := data
	for _, replacement := range r.replacements {
		out = bytes.ReplaceAll(out, []byte(replacement.Prefix), []byte(replacement.Base))
	}
	return out
}

// Stream rewrites prefixes while copying src to dst. Prefixes split across
// reads are held until they can be matched completely.
func (r *Replacer) Stream(dst io.Writer, src io.Reader) error {
	if r == nil || len(r.replacements) == 0 {
		_, err := io.Copy(dst, src)
		return err
	}
	pending := make([]byte, 0, 64*1024)
	readBuf := make([]byte, 32*1024)
	eof := false
	for {
		for len(pending) < r.maxPrefix && !eof {
			n, err := src.Read(readBuf)
			if n > 0 {
				pending = append(pending, readBuf[:n]...)
			}
			if err == io.EOF {
				eof = true
			} else if err != nil {
				return err
			}
		}
		replaced := false
		for _, replacement := range r.replacements {
			prefix := []byte(replacement.Prefix)
			if len(pending) >= len(prefix) && bytes.HasPrefix(pending, prefix) {
				if _, err := dst.Write([]byte(replacement.Base)); err != nil {
					return err
				}
				pending = pending[len(prefix):]
				replaced = true
				break
			}
		}
		if replaced {
			continue
		}
		if eof {
			_, err := dst.Write(pending)
			return err
		}
		partial := false
		for _, replacement := range r.replacements {
			if bytes.HasPrefix([]byte(replacement.Prefix), pending) {
				partial = true
				break
			}
		}
		if partial {
			continue
		}
		if len(pending) == 0 {
			continue
		}
		if _, err := dst.Write(pending[:1]); err != nil {
			return err
		}
		pending = pending[1:]
	}
}

// JSONField rewrites prefixes only when they follow a JSON field value, for
// example `"tarball": "https://.../`.
func (r *Replacer) JSONField(data []byte, field string) ([]byte, error) {
	if r == nil {
		return data, nil
	}
	if field == "" {
		return nil, fmt.Errorf("rewrite field is empty")
	}
	quoted := regexp.QuoteMeta(`"` + field + `"`)
	out := data
	for _, replacement := range r.replacements {
		expression := regexp.MustCompile(`(?m)(` + quoted + `\s*:\s*")(` + regexp.QuoteMeta(replacement.Prefix) + `)`)
		out = expression.ReplaceAllFunc(out, func(match []byte) []byte {
			index := bytes.Index(match, []byte(replacement.Prefix))
			if index < 0 {
				return match
			}
			replaced := make([]byte, 0, len(match)-len(replacement.Prefix)+len(replacement.Base))
			replaced = append(replaced, match[:index]...)
			replaced = append(replaced, replacement.Base...)
			return replaced
		})
	}
	return out, nil
}

// Prefixes returns the configured prefixes in replacement order.
func (r *Replacer) Prefixes() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.replacements))
	for _, replacement := range r.replacements {
		out = append(out, replacement.Prefix)
	}
	return out
}

// Base returns the configured replacement base for a prefix.
func (r *Replacer) Base(prefix string) (string, bool) {
	if r == nil {
		return "", false
	}
	for _, replacement := range r.replacements {
		if replacement.Prefix == prefix {
			return replacement.Base, true
		}
	}
	return "", false
}

// TrimPrefix returns the suffix after the longest matching prefix.
func (r *Replacer) TrimPrefix(value string) (string, bool) {
	if r == nil {
		return "", false
	}
	for _, replacement := range r.replacements {
		if strings.HasPrefix(value, replacement.Prefix) {
			return strings.TrimPrefix(value, replacement.Prefix), true
		}
	}
	return "", false
}
