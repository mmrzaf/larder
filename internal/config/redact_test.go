package config

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	dir := t.TempDir()
	cfg := `
version: 1
data_dir: ` + dir + `
upstream_proxy: http://user:pass@127.0.0.1:3128
repos:
  - name: generic
    kind: generic
    proxy: socks5h://alice:secret@127.0.0.1:1080
    upstreams:
      - url: https://user:upstreampass@example.com/base
        headers:
          Authorization: Bearer topsecret
          X-Api-Key: another-secret
`
	c, err := Load(writeConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	redacted := c.Redact()

	original := c.Repos[0].Upstreams[0].Headers["Authorization"]
	if original != "Bearer topsecret" {
		t.Fatalf("Redact mutated the original header: %q", original)
	}
	if redacted.Repos[0].Upstreams[0].Headers["Authorization"] != "***" {
		t.Fatalf("header not masked: %q", redacted.Repos[0].Upstreams[0].Headers["Authorization"])
	}
	if strings.Contains(redacted.UpstreamProxy.String(), "pass") || strings.Contains(string(redacted.UpstreamProxy), "user") {
		t.Fatalf("global proxy credentials leaked: %q", redacted.UpstreamProxy)
	}
	if strings.Contains(string(redacted.Repos[0].Proxy), "alice") || strings.Contains(string(redacted.Repos[0].Proxy), "secret") {
		t.Fatalf("repo proxy credentials leaked: %q", redacted.Repos[0].Proxy)
	}
	if strings.Contains(redacted.Repos[0].Upstreams[0].URL, "upstreampass") {
		t.Fatalf("upstream URL credentials leaked: %q", redacted.Repos[0].Upstreams[0].URL)
	}
	if !strings.Contains(string(redacted.UpstreamProxy), "***") {
		t.Fatalf("global proxy not marked: %q", redacted.UpstreamProxy)
	}
}

func TestRedactNil(t *testing.T) {
	var c *Config
	if c.Redact() != nil {
		t.Fatal("nil config did not stay nil")
	}
}
