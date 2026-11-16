package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func osReadFileExample() (string, error) {
	data, err := os.ReadFile("../../docs/05-configuration/example.yaml")
	return string(data), err
}

func TestSizeDurationProxyHelpers(t *testing.T) {
	if got := Size(512 << 20).Bytes(); got != 512<<20 {
		t.Fatalf("Bytes = %d", got)
	}
	if got := Duration(5 * time.Second).Duration(); got != 5*time.Second {
		t.Fatalf("Duration = %v", got)
	}
	for _, p := range []Proxy{ProxyDirect, ProxyEnv, ""} {
		u, err := p.URL()
		if err != nil || u != nil {
			t.Fatalf("Proxy(%q).URL() = %v, %v", p, u, err)
		}
	}
	u, err := Proxy("http://127.0.0.1:3128").URL()
	if err != nil || u.Host != "127.0.0.1:3128" {
		t.Fatalf("proxy URL = %v, %v", u, err)
	}
}

func TestErrorsUnwrap(t *testing.T) {
	first := errors.New("first")
	second := errors.New("second")
	errs := Errors{first, second}
	if !errors.Is(errs, first) || !errors.Is(errs, second) {
		t.Fatal("Errors does not unwrap its members")
	}
}

func TestExpandEnvNilLookup(t *testing.T) {
	got, err := ExpandEnv("${MISSING:-fallback}", nil)
	if err != nil || got != "fallback" {
		t.Fatalf("ExpandEnv = %q, %v", got, err)
	}
}

func TestTimeoutOverridesApply(t *testing.T) {
	base := Defaults().Timeouts
	connect := Duration(2 * time.Second)
	header := Duration(3 * time.Second)
	overrides := &TimeoutOverrides{Connect: &connect, ResponseHeader: &header}
	got := overrides.Apply(base)
	if got.Connect != connect || got.ResponseHeader != header || got.Stall != base.Stall {
		t.Fatalf("applied timeouts = %#v", got)
	}
	if (&TimeoutOverrides{}).Apply(base) != base {
		t.Fatal("empty overrides changed the base timeouts")
	}
}

func TestLoadUpstreamTimeouts(t *testing.T) {
	dir := t.TempDir()
	cfg := `
version: 1
data_dir: ` + dir + `
repos:
  - name: go
    kind: go
    upstreams:
      - url: https://example.com
        timeouts:
          connect: 2s
          response_header: 3s
  - name: go2
    kind: go
    upstreams:
      - url: https://example.com
        timeouts: {}
`
	c, err := Load(writeConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	override := c.Repos[0].Upstreams[0].Timeouts
	if override == nil || override.Connect == nil || *override.Connect != Duration(2*time.Second) || override.ResponseHeader == nil {
		t.Fatalf("timeouts = %#v", override)
	}
	if c.Repos[1].Upstreams[0].Timeouts != nil {
		t.Fatal("empty timeout block produced overrides")
	}
}

func TestLoadPrintAllKinds(t *testing.T) {
	data, err := osReadFileExample()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]byte(strings.Replace(data, "data_dir: /data", "data_dir: "+t.TempDir(), 1)))
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, key := range []string{"verify:", "companion_fetch:", "audit:", "files_hosts:", "dl_upstreams:", "suite_consistency:", "rules:", "default_class:", "expect:", "companion:"} {
		if !strings.Contains(text, key) {
			t.Errorf("printed configuration missing %q:\n%s", key, text)
		}
	}
}

func TestRedactAllKinds(t *testing.T) {
	data, err := osReadFileExample()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]byte(strings.Replace(data, "data_dir: /data", "data_dir: "+t.TempDir(), 1)))
	if err != nil {
		t.Fatal(err)
	}
	redacted := c.Redact()
	if redacted == nil || len(redacted.Repos) != len(c.Repos) {
		t.Fatal("redaction lost repositories")
	}
	if redacted.Repos[0].Go == nil || redacted.Repos[1].NPM == nil || redacted.Repos[2].PyPI == nil ||
		redacted.Repos[3].Crates == nil || redacted.Repos[5].Maven == nil || redacted.Repos[8].Generic == nil ||
		redacted.Repos[9].APT == nil {
		t.Fatal("redaction lost kind options")
	}
}

func TestValidationExtras(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"routing", "routing: nope\n", "routing"},
		{"log level", "log:\n  level: verbose\n", "log.level"},
		{"log format", "log:\n  format: xml\n", "log.format"},
		{"sample hits", "log:\n  sample_hits: 2\n", "log.sample_hits"},
		{"limits object", "limits:\n  max_object_size: 0\n", "limits.max_object_size"},
		{"limits background", "limits:\n  background_complete_max: -1\n", "limits.background_complete_max"},
		{"limits fetches", "limits:\n  max_concurrent_fetches: 0\n", "limits.max_concurrent_fetches"},
		{"limits conns", "limits:\n  max_upstream_conns_per_host: 0\n", "limits.max_upstream_conns_per_host"},
		{"limits parse", "limits:\n  max_parse_bytes: 0\n", "limits.max_parse_bytes"},
		{"breaker threshold", "breaker:\n  failure_threshold: 0\n", "breaker.failure_threshold"},
		{"breaker open", "breaker:\n  open_for: 0s\n", "breaker.open_for"},
		{"breaker max", "breaker:\n  open_for: 10s\n  open_max: 5s\n", "breaker.open_max"},
		{"cache history", "cache:\n  metadata_history: -1\n", "cache.metadata_history"},
		{"cache negative", "cache:\n  negative_ttl: -1s\n", "cache.negative_ttl"},
		{"cache stale", "cache:\n  max_stale: -1s\n", "cache.max_stale"},
		{"storage max", "storage:\n  max_bytes: -1\n", "storage.max_bytes"},
		{"storage low", "storage:\n  low_watermark: -0.1\n", "storage.low_watermark"},
		{"storage high", "storage:\n  high_watermark: 0\n", "storage.high_watermark"},
		{"storage gc", "storage:\n  gc_interval: 0s\n", "storage.gc_interval"},
		{"storage gc max", "storage:\n  gc_max_duration: 0s\n", "storage.gc_max_duration"},
		{"storage policy", "storage:\n  eviction_policy: fifo\n", "storage.eviction_policy"},
		{"storage days", "storage:\n  protect_recent_days: -1\n", "storage.protect_recent_days"},
		{"storage unused", "storage:\n  metadata_unused_days: -1\n", "storage.metadata_unused_days"},
		{"storage quarantine", "storage:\n  quarantine_keep_days: -1\n", "storage.quarantine_keep_days"},
		{"storage snapshots", "storage:\n  snapshot_keep: -1\n", "storage.snapshot_keep"},
		{"storage scrub percent", "storage:\n  scrub:\n    daily_percent: 101\n", "storage.scrub.daily_percent"},
		{"storage scrub rate", "storage:\n  scrub:\n    rate_mb_s: -1\n", "storage.scrub.rate_mb_s"},
		{"storage protect", "storage:\n  protect:\n    - {repo: missing, match: x}\n", "storage.protect[0].repo"},
		{"host duplicate", "routing: host\nrepos:\n  - {name: a, kind: generic, host: x, upstreams: [{url: https://example.com}]}\n  - {name: b, kind: generic, host: x, upstreams: [{url: https://example.com}]}\n", "duplicate host"},
		{"repo proxy", "repos:\n  - {name: a, kind: generic, proxy: ftp://x, upstreams: [{url: https://example.com}]}\n", "repos[0].proxy"},
		{"repo public", "repos:\n  - {name: a, kind: generic, public_url: ftp://x, upstreams: [{url: https://example.com}]}\n", "repos[0].public_url"},
		{"repo ttl", "repos:\n  - {name: a, kind: generic, revalidate_timeout: 0s, upstreams: [{url: https://example.com}]}\n", "revalidate_timeout"},
		{"repo max", "repos:\n  - {name: a, kind: generic, max_object_size: 0, upstreams: [{url: https://example.com}]}\n", "max_object_size"},
		{"repo quota", "repos:\n  - {name: a, kind: generic, quota_bytes: -1, upstreams: [{url: https://example.com}]}\n", "quota_bytes"},
		{"repo weight", "repos:\n  - {name: a, kind: generic, evict_weight: 0, upstreams: [{url: https://example.com}]}\n", "evict_weight"},
		{"repo unused", "repos:\n  - {name: a, kind: generic, unused_days: -1, upstreams: [{url: https://example.com}]}\n", "unused_days"},
		{"repo protect", "repos:\n  - {name: a, kind: generic, protect: [''], upstreams: [{url: https://example.com}]}\n", "protect[0]"},
		{"upstream connect", "repos:\n  - {name: a, kind: generic, upstreams: [{url: https://example.com, connect_to: bad}]}\n", "connect_to"},
		{"upstream timeout", "repos:\n  - {name: a, kind: generic, upstreams: [{url: https://example.com, timeouts: {connect: 0s}}]}\n", "timeouts.connect"},
		{"go verify", "repos:\n  - {name: a, kind: go, verify: fancy, upstreams: [{url: https://example.com}]}\n", "verify"},
		{"go sumdb", "repos:\n  - {name: a, kind: go, sumdb_upstreams: {sum: ['ftp://x']}, upstreams: [{url: https://example.com}]}\n", "sumdb_upstreams.sum"},
		{"npm audit", "repos:\n  - {name: a, kind: npm, audit: nope, upstreams: [{url: https://example.com}]}\n", "audit"},
		{"npm timeout", "repos:\n  - {name: a, kind: npm, audit_timeout: 0s, upstreams: [{url: https://example.com}]}\n", "audit_timeout"},
		{"pypi host", "repos:\n  - {name: a, kind: pypi, files_hosts: ['bad/host'], upstreams: [{url: https://example.com}]}\n", "files_hosts"},
		{"crates empty", "repos:\n  - {name: a, kind: crates, dl_upstreams: [], upstreams: [{url: https://example.com}]}\n", "dl_upstreams"},
		{"generic expect", "repos:\n  - {name: a, kind: generic, expect: [{path: /x}], upstreams: [{url: https://example.com}]}\n", "at least one digest"},
		{"generic companion", "repos:\n  - {name: a, kind: generic, companion: [{suffix: sha, algo: crc}], upstreams: [{url: https://example.com}]}\n", "suffix"},
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
