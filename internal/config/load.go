// Package config loads, validates and renders the Larder configuration.
package config

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads, expands, decodes and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration %s: %w", path, err)
	}
	return Parse(data)
}

// Parse decodes and validates configuration bytes.
func Parse(data []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, Errors{fmt.Errorf("parse configuration: %w", err)}
	}
	if len(doc.Content) == 0 {
		return nil, Errors{fmt.Errorf("configuration is empty")}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, Errors{fmt.Errorf("configuration must be a mapping")}
	}
	if err := expandNode(root, os.LookupEnv); err != nil {
		return nil, Errors{err}
	}

	var errs Errors
	checkUnknownKeys(root, reflect.TypeOf(rawConfig{}), "", &errs)

	var raw rawConfig
	if err := root.Decode(&raw); err != nil {
		errs.add("decode configuration: %v", err)
	}
	c, resolveErrs := resolve(&raw)
	errs = append(errs, resolveErrs...)
	applyEnvOverrides(c)
	errs = append(errs, validate(c, &raw)...)

	if len(errs) > 0 {
		return nil, errs
	}
	return c, nil
}

func expandNode(node *yaml.Node, lookup lookupFunc) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode {
		return expandNode(node.Alias, lookup)
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		expanded, err := expandEnv(node.Value, lookup)
		if err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		node.Value = expanded
		if node.Style == 0 {
			node.Tag = inferScalarTag(expanded)
		} else {
			node.Tag = "!!str"
		}
		return nil
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := expandNode(node.Content[i+1], lookup); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := expandNode(child, lookup); err != nil {
			return err
		}
	}
	return nil
}

func checkUnknownKeys(node *yaml.Node, typ reflect.Type, path string, errs *Errors) {
	if node == nil {
		return
	}
	if node.Kind == yaml.AliasNode {
		checkUnknownKeys(node.Alias, typ, path, errs)
		return
	}
	typ = derefType(typ)

	switch node.Kind {
	case yaml.MappingNode:
		switch typ.Kind() {
		case reflect.Struct:
			fields := yamlFields(typ)
			for i := 0; i+1 < len(node.Content); i += 2 {
				key := node.Content[i]
				value := node.Content[i+1]
				field, ok := fields[key.Value]
				if !ok {
					suggestion := closestKey(key.Value, fields)
					p := joinPath(path, key.Value)
					msg := fmt.Sprintf("%s: unknown key (line %d)", p, key.Line)
					if suggestion != "" {
						msg += fmt.Sprintf("; did you mean %q?", suggestion)
					}
					errs.add("%s", msg)
					continue
				}
				checkUnknownKeys(value, field.Type, joinPath(path, key.Value), errs)
			}
		case reflect.Map:
			for i := 0; i+1 < len(node.Content); i += 2 {
				key := node.Content[i]
				value := node.Content[i+1]
				checkUnknownKeys(value, typ.Elem(), joinPath(path, key.Value), errs)
			}
		}
	case yaml.SequenceNode:
		if typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
			for i, child := range node.Content {
				checkUnknownKeys(child, typ.Elem(), fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
	}
}

func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func yamlFields(t reflect.Type) map[string]reflect.StructField {
	fields := make(map[string]reflect.StructField, t.NumField())
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
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		if name == "" {
			continue
		}
		fields[name] = field
	}
	return fields
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func closestKey(key string, fields map[string]reflect.StructField) string {
	best := ""
	bestDistance := 3
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		distance := levenshtein(key, name)
		if distance < bestDistance {
			best = name
			bestDistance = distance
		}
	}
	if bestDistance > 2 {
		return ""
	}
	return best
}

func levenshtein(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := 0; j <= len(br); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

func inferScalarTag(value string) string {
	if value == "" {
		return "!!str"
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return "!!int"
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return "!!float"
	}
	if _, err := strconv.ParseBool(value); err == nil {
		return "!!bool"
	}
	return "!!str"
}
