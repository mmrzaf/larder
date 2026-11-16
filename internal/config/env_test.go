package config

import "testing"

func TestExpandEnv(t *testing.T) {
	env := map[string]string{
		"NAME":  "larder",
		"EMPTY": "",
		"OTHER": "value",
	}
	lookup := func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}

	tests := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"${NAME}", "larder"},
		{"pre-${NAME}-post", "pre-larder-post"},
		{"${MISSING}", ""},
		{"${MISSING:-fallback}", "fallback"},
		{"${EMPTY:-fallback}", "fallback"},
		{"${NAME:-fallback}", "larder"},
		{"${MISSING:-${OTHER}}", "value"},
		{"$NAME", "$NAME"},
		{"$${NAME}", "$larder"},
	}
	for _, tt := range tests {
		got, err := ExpandEnv(tt.in, lookup)
		if err != nil {
			t.Fatalf("ExpandEnv(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("ExpandEnv(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExpandEnvErrors(t *testing.T) {
	for _, input := range []string{"${NAME", "${}", "${1NAME}", "${NAME-}}"} {
		if _, err := ExpandEnv(input, func(string) (string, bool) { return "", false }); err == nil {
			t.Errorf("ExpandEnv(%q) succeeded, want error", input)
		}
	}
}

func FuzzExpandEnv(f *testing.F) {
	for _, seed := range []string{"", "${NAME}", "${NAME:-fallback}", "${", "}", "$${NAME}", "${MISSING:-${OTHER}}"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = ExpandEnv(input, func(key string) (string, bool) {
			if key == "NAME" {
				return "larder", true
			}
			return "", false
		})
	})
}

func FuzzInferScalarTag(f *testing.F) {
	for _, seed := range []string{"", "1", "-1", "1.5", "true", "larder", "512MiB", "0x10"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		switch got := inferScalarTag(input); got {
		case "!!str", "!!int", "!!float", "!!bool":
		default:
			t.Fatalf("inferScalarTag(%q) = %q", input, got)
		}
	})
}
