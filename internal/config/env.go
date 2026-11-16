package config

import (
	"fmt"
	"strings"
)

type lookupFunc func(string) (string, bool)

// ExpandEnv replaces ${NAME} and ${NAME:-default} references in s.
func ExpandEnv(s string, lookup lookupFunc) (string, error) {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	return expandEnv(s, lookup)
}

func expandEnv(s string, lookup lookupFunc) (string, error) {
	var out strings.Builder
	out.Grow(len(s))

	for i := 0; i < len(s); {
		if s[i] != '$' || i+1 >= len(s) || s[i+1] != '{' {
			out.WriteByte(s[i])
			i++
			continue
		}

		end := closingBrace(s, i+2)
		if end < 0 {
			return "", fmt.Errorf("unterminated environment reference at offset %d", i)
		}
		expr := s[i+2 : end]
		name, fallback, hasFallback := strings.Cut(expr, ":-")
		if !validEnvName(name) {
			return "", fmt.Errorf("invalid environment reference %q", s[i:end+1])
		}

		value, ok := lookup(name)
		if !ok || value == "" {
			if hasFallback {
				expanded, err := expandEnv(fallback, lookup)
				if err != nil {
					return "", err
				}
				value = expanded
			} else {
				value = ""
			}
		}
		out.WriteString(value)
		i = end + 1
	}
	return out.String(), nil
}

func closingBrace(s string, start int) int {
	depth := 1
	for i := start; i < len(s); i++ {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			depth++
			i++
			continue
		}
		if s[i] == '}' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}
