package config

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestEveryConfigKeyIsDocumented(t *testing.T) {
	data, err := os.ReadFile("../../docs/05-configuration/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	document := string(data)

	keys := make(map[string]bool)
	collectYAMLKeys(reflect.TypeOf(rawConfig{}), keys)
	for key := range keys {
		pattern := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(key) + `([^A-Za-z0-9_]|$)`)
		if !pattern.MatchString(document) {
			t.Errorf("config key %q is missing from docs/05-configuration/reference.md", key)
		}
	}
}

func collectYAMLKeys(t reflect.Type, keys map[string]bool) {
	t = derefType(t)
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.PkgPath != "" {
				continue
			}
			tag := field.Tag.Get("yaml")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if name != "" {
				keys[name] = true
			}
			collectYAMLKeys(field.Type, keys)
		}
	case reflect.Slice, reflect.Array:
		collectYAMLKeys(t.Elem(), keys)
	case reflect.Map:
		collectYAMLKeys(t.Elem(), keys)
	}
}
