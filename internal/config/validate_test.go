package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMountsOverlap(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"/maven", "/maven", true},
		{"/maven", "/maven/central", true},
		{"/maven/central", "/maven", true},
		{"/maven/central", "/maven/google", false},
		{"/go", "/golang", false},
		{"/", "/npm", true},
		{"/npm", "/npm/", true},
	}
	for _, tt := range tests {
		if got := mountsOverlap(tt.a, tt.b); got != tt.want {
			t.Errorf("mountsOverlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestValidationRules(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "version",
			body: "version: 2\n",
			want: "version: must be 1",
		},
		{
			name: "name",
			body: "repos:\n  - name: bad name\n    kind: generic\n    upstreams: [{url: https://example.com}]\n",
			want: "repos[0].name",
		},
		{
			name: "kind",
			body: "repos:\n  - name: x\n    kind: unknown\n    upstreams: [{url: https://example.com}]\n",
			want: "repos[0].kind",
		},
		{
			name: "mount",
			body: "repos:\n  - name: x\n    kind: generic\n    mount: relative\n    upstreams: [{url: https://example.com}]\n",
			want: "repos[0].mount",
		},
		{
			name: "watermarks",
			body: "storage:\n  low_watermark: 0.95\n  high_watermark: 0.90\n",
			want: "storage.low_watermark",
		},
		{
			name: "free space",
			body: "storage:\n  hard_free_bytes: 2GiB\n  min_free_bytes: 1GiB\n",
			want: "storage.hard_free_bytes",
		},
		{
			name: "offline status",
			body: "offline_miss_status: 418\n",
			want: "offline_miss_status",
		},
		{
			name: "regex",
			body: "repos:\n  - name: x\n    kind: generic\n    upstreams: [{url: https://example.com}]\n    rules:\n      - {match: '[', class: immutable}\n",
			want: "repos[0].rules[0].match",
		},
		{
			name: "url",
			body: "repos:\n  - name: x\n    kind: generic\n    upstreams: [{url: ftp://example.com}]\n",
			want: "repos[0].upstreams[0].url",
		},
		{
			name: "host routing",
			body: "routing: host\nrepos:\n  - name: x\n    kind: generic\n    upstreams: [{url: https://example.com}]\n",
			want: "repos[0].host",
		},
		{
			name: "upstreams",
			body: "repos:\n  - name: x\n    kind: generic\n",
			want: "repos[0].upstreams",
		},
		{
			name: "proxy",
			body: "upstream_proxy: ftp://proxy\n",
			want: "upstream_proxy",
		},
		{
			name: "cidr",
			body: "access:\n  allow_cidrs: [10.0.0.0/33]\n",
			want: "access.allow_cidrs",
		},
		{
			name: "generic default class",
			body: "repos:\n  - name: x\n    kind: generic\n    upstreams: [{url: https://example.com}]\n    default_class: immutable\n",
			want: "repos[0].default_class",
		},
		{
			name: "generic digest",
			body: "repos:\n  - name: x\n    kind: generic\n    upstreams: [{url: https://example.com}]\n    expect:\n      - {path: /x, sha256: abc}\n",
			want: "repos[0].expect[0].sha256",
		},
		{
			name: "crates template",
			body: "repos:\n  - name: x\n    kind: crates\n    upstreams: [{url: https://index.crates.io}]\n    dl_upstreams: ['https://static.crates.io/crates/x']\n",
			want: "dl_upstreams",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := "version: 1\ndata_dir: " + dir + "\n" + tt.body
			_, err := Load(writeConfig(t, cfg))
			errs := requireConfigErrors(t, err)
			if !strings.Contains(errs.Error(), tt.want) {
				t.Fatalf("errors do not contain %q:\n%s", tt.want, errs)
			}
		})
	}
}

func TestValidateDataDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := Load(writeConfig(t, "version: 1\ndata_dir: "+file+"\n"))
	if err == nil || !strings.Contains(err.Error(), "data_dir: not a directory") {
		t.Fatalf("unexpected error: %v", err)
	}

	missing := filepath.Join(dir, "nested", "data")
	if _, err := Load(writeConfig(t, "version: 1\ndata_dir: "+missing+"\n")); err != nil {
		t.Fatalf("missing directory with writable parent rejected: %v", err)
	}
}

func TestValidateDuplicateRepoNameAndMount(t *testing.T) {
	dir := t.TempDir()
	cfg := `
version: 1
data_dir: ` + dir + `
repos:
  - name: one
    kind: generic
    mount: /same
    upstreams: [{url: https://example.com}]
  - name: one
    kind: generic
    mount: /same
    upstreams: [{url: https://example.com}]
`
	_, err := Load(writeConfig(t, cfg))
	errs := requireConfigErrors(t, err)
	text := errs.Error()
	if !strings.Contains(text, "duplicate repo name") || !strings.Contains(text, "overlaps") {
		t.Fatalf("unexpected errors:\n%s", text)
	}
}
