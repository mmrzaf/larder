package config

import (
	"strings"
	"testing"
)

func TestParseSyntaxError(t *testing.T) {
	if _, err := Parse([]byte("version: [\n")); err == nil || !strings.Contains(err.Error(), "parse configuration") {
		t.Fatalf("syntax error = %v", err)
	}
}

func TestParseTypeError(t *testing.T) {
	_, err := Parse([]byte("version: 1\ndata_dir: /tmp\ntrust_forwarded: not-a-bool\n"))
	errs := requireConfigErrors(t, err)
	if !strings.Contains(errs.Error(), "cannot unmarshal") {
		t.Fatalf("type error = %v", errs)
	}
}
