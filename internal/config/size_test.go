package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		input string
		want  Size
	}{
		{"0", 0},
		{"123", 123},
		{"1B", 1},
		{"512MiB", 512 << 20},
		{"20GiB", 20 << 30},
		{"1TiB", 1 << 40},
		{"64KiB", 64 << 10},
		{" 5GiB ", 5 << 30},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseSize(tt.input)
			if err != nil {
				t.Fatalf("ParseSize(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseSizeErrors(t *testing.T) {
	for _, input := range []string{"", "-1", "1.5GiB", "12XB", "abc", "9223372036854775808", "999999999999PiB"} {
		if _, err := ParseSize(input); err == nil {
			t.Errorf("ParseSize(%q) succeeded, want error", input)
		}
	}
}

func TestSizeString(t *testing.T) {
	tests := []struct {
		in   Size
		want string
	}{
		{0, "0"},
		{512 << 20, "512MiB"},
		{1 << 20, "1MiB"},
		{1536, "1536B"},
		{1 << 40, "1TiB"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Size(%d).String() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func FuzzParseSize(f *testing.F) {
	for _, seed := range []string{"", "0", "512MiB", "20GiB", "1TiB", "1536", "-1", "1.5GiB", "999999999999PiB"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := ParseSize(input)
		if err != nil {
			return
		}
		if got < 0 {
			t.Fatalf("ParseSize(%q) returned negative size %d", input, got)
		}
		round, err := ParseSize(got.String())
		if err != nil {
			t.Fatalf("ParseSize(%q) roundtrip: %v", got.String(), err)
		}
		if round != got {
			t.Fatalf("ParseSize(%q) roundtrip = %d, want %d", got.String(), round, got)
		}
	})
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input string
		want  Duration
	}{
		{"0", 0},
		{"500ms", 500_000_000},
		{"6s", 6_000_000_000},
		{"5m", 300_000_000_000},
		{"24h", 86_400_000_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseDuration(tt.input)
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseDuration(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseDurationErrors(t *testing.T) {
	for _, input := range []string{"", "-1s", "5", "soon", "1d"} {
		if _, err := ParseDuration(input); err == nil {
			t.Errorf("ParseDuration(%q) succeeded, want error", input)
		}
	}
}

func FuzzParseDuration(f *testing.F) {
	for _, seed := range []string{"", "0", "500ms", "6s", "5m", "24h", "-1s", "soon"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got, err := ParseDuration(input)
		if err != nil {
			return
		}
		round, err := ParseDuration(got.String())
		if err != nil {
			t.Fatalf("ParseDuration(%q) roundtrip: %v", got.String(), err)
		}
		if round != got {
			t.Fatalf("ParseDuration(%q) roundtrip = %d, want %d", got.String(), round, got)
		}
	})
}

func TestSizeYAMLRoundtrip(t *testing.T) {
	type holder struct {
		Size Size `yaml:"size"`
	}
	in := holder{Size: 512 << 20}
	data, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out holder
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("roundtrip = %#v, want %#v", out, in)
	}
}
