package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Size is a byte count. It accepts binary units such as 512MiB and bare bytes.
type Size int64

var sizeUnits = []struct {
	suffix string
	scale  int64
}{
	{"PiB", 1 << 50},
	{"TiB", 1 << 40},
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
}

// ParseSize parses a size such as 512MiB, 20GiB, 1TiB or a bare byte count.
func ParseSize(input string) (Size, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("size must not be negative")
	}

	digits := 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, fmt.Errorf("invalid size %q", input)
	}

	n, err := strconv.ParseInt(s[:digits], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", input, err)
	}

	suffix := s[digits:]
	if suffix == "" || strings.EqualFold(suffix, "B") {
		return Size(n), nil
	}

	for _, unit := range sizeUnits {
		if !strings.EqualFold(suffix, unit.suffix) {
			continue
		}
		if n > math.MaxInt64/unit.scale {
			return 0, fmt.Errorf("size %q overflows int64", input)
		}
		return Size(n * unit.scale), nil
	}
	return 0, fmt.Errorf("unknown size unit %q", suffix)
}

// String formats the size with the largest binary unit that represents it exactly.
func (s Size) String() string {
	if s == 0 {
		return "0"
	}
	for _, unit := range sizeUnits {
		if int64(s)%unit.scale == 0 {
			return strconv.FormatInt(int64(s)/unit.scale, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(int64(s), 10) + "B"
}

// Bytes returns the size as a signed byte count.
func (s Size) Bytes() int64 { return int64(s) }

// MarshalYAML renders the size as a human-readable string.
func (s Size) MarshalYAML() (any, error) {
	if s == 0 {
		return 0, nil
	}
	return s.String(), nil
}

// UnmarshalYAML parses a size from a YAML scalar.
func (s *Size) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("size must be a scalar")
	}
	if value.Tag == "!!null" {
		*s = 0
		return nil
	}
	parsed, err := ParseSize(value.Value)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}
