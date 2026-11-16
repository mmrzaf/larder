package config

import (
	"bytes"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultYAMLContainsEveryDefault(t *testing.T) {
	data, err := DefaultYAML()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("# ")) {
		t.Fatal("default configuration is not annotated")
	}
	if !bytes.Contains(data, []byte("repos: []")) {
		t.Fatalf("default configuration has no repos key:\n%s", data)
	}
	for _, want := range []string{"name required", "quota_bytes 0", "require_verified false", "sumdb_upstreams derived", "files_hosts [files.pythonhosted.org]"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("default configuration does not document %q", want)
		}
	}

	var got Config
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("default configuration does not parse: %v\n%s", err, data)
	}
	want := *Defaults()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default configuration roundtrip differs\n got: %#v\nwant: %#v", got, want)
	}
}
