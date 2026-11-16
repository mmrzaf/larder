package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time interval that accepts Go duration syntax.
type Duration time.Duration

// ParseDuration parses a non-negative duration such as 500ms, 6s, 5m or 24h.
func ParseDuration(input string) (Duration, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", input, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must not be negative")
	}
	return Duration(d), nil
}

// String formats the duration using Go duration syntax.
func (d Duration) String() string { return time.Duration(d).String() }

// Duration returns the standard library representation.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// MarshalYAML renders the duration as a human-readable string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML parses a duration from a YAML scalar.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	if value.Tag == "!!null" {
		*d = 0
		return nil
	}
	parsed, err := ParseDuration(value.Value)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
