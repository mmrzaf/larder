package config

import (
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validate(c *Config, raw *rawConfig) Errors {
	var errs Errors

	if c.Version != 1 {
		errs.add("version: must be 1")
	}
	if c.DataDir == "" {
		errs.add("data_dir: required")
	} else if err := validateDataDir(c.DataDir); err != nil {
		errs.add("data_dir: %v", err)
	}
	if err := validateListen(c.Listen, false); err != nil {
		errs.add("listen: %v", err)
	}
	if err := validateListen(c.Admin.Listen, true); err != nil {
		errs.add("admin.listen: %v", err)
	}
	switch c.Routing {
	case RoutingPath, RoutingHost, RoutingBoth:
	default:
		errs.add("routing: must be path, host or both")
	}
	switch c.OfflineMissStatus {
	case 404, 502, 503, 504:
	default:
		errs.add("offline_miss_status: must be 404, 502, 503 or 504")
	}
	if err := validateHTTPURL("public_base_url", c.PublicBaseURL, true); err != nil {
		errs.add("public_base_url: %v", err)
	}
	if err := validateProxy(c.UpstreamProxy); err != nil {
		errs.add("upstream_proxy: %v", err)
	}
	if c.MaxHeaderBytes <= 0 {
		errs.add("max_header_bytes: must be greater than zero")
	}
	if c.ShutdownGrace <= 0 {
		errs.add("shutdown_grace: must be greater than zero")
	}
	for _, cidr := range c.Access.AllowCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			errs.add("access.allow_cidrs: %q: %v", cidr, err)
		}
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs.add("log.level: must be debug, info, warn or error")
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		errs.add("log.format: must be json or text")
	}
	if c.Log.SampleHits < 0 || c.Log.SampleHits > 1 {
		errs.add("log.sample_hits: must be between 0 and 1")
	}
	validateTimeouts("timeouts", c.Timeouts, &errs)
	if c.Limits.MaxObjectSize <= 0 {
		errs.add("limits.max_object_size: must be greater than zero")
	}
	if c.Limits.BackgroundCompleteMax < 0 {
		errs.add("limits.background_complete_max: must not be negative")
	}
	if c.Limits.MaxConcurrentFetches <= 0 {
		errs.add("limits.max_concurrent_fetches: must be greater than zero")
	}
	if c.Limits.MaxUpstreamConnsPerHost <= 0 {
		errs.add("limits.max_upstream_conns_per_host: must be greater than zero")
	}
	if c.Limits.MaxParseBytes <= 0 {
		errs.add("limits.max_parse_bytes: must be greater than zero")
	}
	if c.Breaker.FailureThreshold <= 0 {
		errs.add("breaker.failure_threshold: must be greater than zero")
	}
	if c.Breaker.OpenFor <= 0 {
		errs.add("breaker.open_for: must be greater than zero")
	}
	if c.Breaker.OpenMax < c.Breaker.OpenFor {
		errs.add("breaker.open_max: must not be smaller than open_for")
	}
	if c.Cache.MetadataHistory < 0 {
		errs.add("cache.metadata_history: must not be negative")
	}
	if c.Cache.NegativeTTL < 0 {
		errs.add("cache.negative_ttl: must not be negative")
	}
	if c.Cache.StaleWhileRevalidate < 0 {
		errs.add("cache.stale_while_revalidate: must not be negative")
	}
	if c.Cache.MaxStale < 0 {
		errs.add("cache.max_stale: must not be negative")
	}
	if c.Storage.MaxBytes < 0 {
		errs.add("storage.max_bytes: must not be negative")
	}
	if c.Storage.MinFreeBytes < 0 {
		errs.add("storage.min_free_bytes: must not be negative")
	}
	if c.Storage.HardFreeBytes < 0 {
		errs.add("storage.hard_free_bytes: must not be negative")
	}
	if c.Storage.HardFreeBytes > c.Storage.MinFreeBytes {
		errs.add("storage.hard_free_bytes: must not be greater than min_free_bytes")
	}
	if c.Storage.LowWatermark < 0 || c.Storage.LowWatermark >= 1 {
		errs.add("storage.low_watermark: must be at least 0 and less than 1")
	}
	if c.Storage.HighWatermark <= 0 || c.Storage.HighWatermark > 1 {
		errs.add("storage.high_watermark: must be greater than 0 and at most 1")
	}
	if c.Storage.LowWatermark >= c.Storage.HighWatermark {
		errs.add("storage.low_watermark: must be smaller than high_watermark")
	}
	if c.Storage.GCInterval <= 0 {
		errs.add("storage.gc_interval: must be greater than zero")
	}
	if c.Storage.GCMaxDuration <= 0 {
		errs.add("storage.gc_max_duration: must be greater than zero")
	}
	switch c.Storage.EvictionPolicy {
	case "lru", "lfu", "score":
	default:
		errs.add("storage.eviction_policy: must be lru, lfu or score")
	}
	if c.Storage.ProtectRecentDays < 0 {
		errs.add("storage.protect_recent_days: must not be negative")
	}
	if c.Storage.MetadataUnusedDays < 0 {
		errs.add("storage.metadata_unused_days: must not be negative")
	}
	if c.Storage.QuarantineKeepDays < 0 {
		errs.add("storage.quarantine_keep_days: must not be negative")
	}
	if c.Storage.SnapshotKeep < 0 {
		errs.add("storage.snapshot_keep: must not be negative")
	}
	if c.Storage.Scrub.DailyPercent < 0 || c.Storage.Scrub.DailyPercent > 100 {
		errs.add("storage.scrub.daily_percent: must be between 0 and 100")
	}
	if c.Storage.Scrub.RateMBs < 0 {
		errs.add("storage.scrub.rate_mb_s: must not be negative")
	}

	repoNames := make(map[string]int, len(c.Repos))
	for i := range c.Repos {
		repoNames[c.Repos[i].Name] = i
	}
	for i := range c.Storage.Protect {
		p := c.Storage.Protect[i]
		path := fmt.Sprintf("storage.protect[%d]", i)
		if p.Repo == "" {
			errs.add("%s.repo: required", path)
		} else if _, ok := repoNames[p.Repo]; !ok {
			errs.add("%s.repo: unknown repo %q", path, p.Repo)
		}
		if p.Match == "" {
			errs.add("%s.match: required", path)
		}
	}

	validateRepos(c, raw, &errs)
	return errs
}

func validateRepos(c *Config, raw *rawConfig, errs *Errors) {
	names := make(map[string]bool, len(c.Repos))
	hosts := make(map[string]bool, len(c.Repos))

	for i := range c.Repos {
		r := &c.Repos[i]
		rawRepo := &raw.Repos[i]
		p := fmt.Sprintf("repos[%d]", i)

		if r.Name == "" {
			errs.add("%s.name: required", p)
		} else if !repoNamePattern.MatchString(r.Name) {
			errs.add("%s.name: must contain only letters, digits, '.', '_' and '-'", p)
		} else if names[r.Name] {
			errs.add("%s.name: duplicate repo name %q", p, r.Name)
		}
		names[r.Name] = true

		switch r.Kind {
		case "go", "maven", "npm", "pypi", "crates", "rustup", "apt", "alpine", "generic":
		case "":
			errs.add("%s.kind: required", p)
		default:
			errs.add("%s.kind: unknown kind %q", p, r.Kind)
		}

		if err := validateMount(r.Mount); err != nil {
			errs.add("%s.mount: %v", p, err)
		}
		if c.Routing == RoutingHost || c.Routing == RoutingBoth {
			if r.Host == "" {
				errs.add("%s.host: required for host routing", p)
			}
		}
		if r.Host != "" {
			if err := validateHostname(r.Host); err != nil {
				errs.add("%s.host: %v", p, err)
			}
			if hosts[r.Host] {
				errs.add("%s.host: duplicate host %q", p, r.Host)
			}
			hosts[r.Host] = true
		}
		if err := validateHTTPURL(p+".public_url", r.PublicURL, true); err != nil {
			errs.add("%s.public_url: %v", p, err)
		}
		if err := validateProxy(r.Proxy); err != nil {
			errs.add("%s.proxy: %v", p, err)
		}
		if r.FreshTTL < 0 {
			errs.add("%s.fresh_ttl: must not be negative", p)
		}
		if r.RevalidateTimeout <= 0 {
			errs.add("%s.revalidate_timeout: must be greater than zero", p)
		}
		if r.NegativeTTL < 0 {
			errs.add("%s.negative_ttl: must not be negative", p)
		}
		if r.MaxObjectSize <= 0 {
			errs.add("%s.max_object_size: must be greater than zero", p)
		}
		if r.QuotaBytes < 0 {
			errs.add("%s.quota_bytes: must not be negative", p)
		}
		if r.EvictWeight <= 0 {
			errs.add("%s.evict_weight: must be greater than zero", p)
		}
		if r.UnusedDays < 0 {
			errs.add("%s.unused_days: must not be negative", p)
		}
		for j, pattern := range r.Protect {
			if pattern == "" {
				errs.add("%s.protect[%d]: must not be empty", p, j)
			}
		}

		if len(r.Upstreams) == 0 {
			errs.add("%s.upstreams: at least one upstream is required", p)
		}
		for j := range r.Upstreams {
			u := &r.Upstreams[j]
			up := fmt.Sprintf("%s.upstreams[%d]", p, j)
			if err := validateHTTPURL(up+".url", u.URL, false); err != nil {
				errs.add("%s.url: %v", up, err)
			}
			if err := validateProxy(u.Proxy); err != nil {
				errs.add("%s.proxy: %v", up, err)
			}
			if u.ConnectTo != "" {
				if err := validateHostPort(u.ConnectTo); err != nil {
					errs.add("%s.connect_to: %v", up, err)
				}
			}
			if u.CAFile != "" {
				if err := validateCAFile(u.CAFile); err != nil {
					errs.add("%s.ca_file: %v", up, err)
				}
			}
			if u.Timeouts != nil {
				validateTimeoutOverrides(up+".timeouts", u.Timeouts, errs)
			}
		}

		validateKindSpecific(r, rawRepo, p, errs)
	}

	if c.Routing != RoutingHost {
		for i := 0; i < len(c.Repos); i++ {
			for j := i + 1; j < len(c.Repos); j++ {
				if mountsOverlap(c.Repos[i].Mount, c.Repos[j].Mount) {
					errs.add("repos[%d].mount: %q overlaps repos[%d].mount %q",
						i, c.Repos[i].Mount, j, c.Repos[j].Mount)
				}
			}
		}
	}
}

func validateKindSpecific(r *Repo, raw *rawRepo, p string, errs *Errors) {
	wrongKind := func(key string, present bool, expected string) {
		if present && r.Kind != expected {
			errs.add("%s.%s: only valid for kind %s", p, key, expected)
		}
	}
	wrongKind("verify", raw.Verify != nil, "go")
	wrongKind("sumdb_upstreams", raw.SumDBUpstreams != nil, "go")
	wrongKind("companion_fetch", raw.CompanionFetch != nil, "maven")
	wrongKind("audit", raw.Audit != nil, "npm")
	wrongKind("audit_timeout", raw.AuditTimeout != nil, "npm")
	wrongKind("files_hosts", raw.FilesHosts != nil, "pypi")
	wrongKind("dl_upstreams", raw.DLUpstreams != nil, "crates")
	wrongKind("suite_consistency", raw.SuiteConsistency != nil, "apt")
	wrongKind("rules", raw.Rules != nil, "generic")
	wrongKind("default_class", raw.DefaultClass != nil, "generic")
	wrongKind("keep_query", raw.KeepQuery != nil, "generic")
	wrongKind("reject_html", raw.RejectHTML != nil, "generic")
	wrongKind("expect", raw.Expect != nil, "generic")
	wrongKind("companion", raw.Companion != nil, "generic")

	switch r.Kind {
	case "go":
		if r.Go == nil {
			return
		}
		switch r.Go.Verify {
		case "basic", "strict":
		default:
			errs.add("%s.verify: must be basic or strict", p)
		}
		for name, urls := range r.Go.SumDBUpstreams {
			if name == "" {
				errs.add("%s.sumdb_upstreams: name must not be empty", p)
			}
			for _, u := range urls {
				if err := validateHTTPURL(p+".sumdb_upstreams."+name, u, false); err != nil {
					errs.add("%s.sumdb_upstreams.%s: %v", p, name, err)
				}
			}
		}
	case "maven":
	case "npm":
		if r.NPM == nil {
			return
		}
		switch r.NPM.Audit {
		case "passthrough", "stub", "deny":
		default:
			errs.add("%s.audit: must be passthrough, stub or deny", p)
		}
		if r.NPM.AuditTimeout <= 0 {
			errs.add("%s.audit_timeout: must be greater than zero", p)
		}
	case "pypi":
		if r.PyPI == nil {
			return
		}
		for _, host := range r.PyPI.FilesHosts {
			if err := validateHostname(host); err != nil {
				errs.add("%s.files_hosts: %v", p, err)
			}
		}
	case "crates":
		if r.Crates == nil {
			return
		}
		if len(r.Crates.DLUpstreams) == 0 {
			errs.add("%s.dl_upstreams: at least one template is required", p)
		}
		for _, tmpl := range r.Crates.DLUpstreams {
			if !strings.Contains(tmpl, "{crate}") || !strings.Contains(tmpl, "{version}") {
				errs.add("%s.dl_upstreams: %q must contain {crate} and {version}", p, tmpl)
				continue
			}
			rendered := strings.ReplaceAll(tmpl, "{crate}", "example")
			rendered = strings.ReplaceAll(rendered, "{version}", "1.0.0")
			if err := validateHTTPURL(p+".dl_upstreams", rendered, false); err != nil {
				errs.add("%s.dl_upstreams: %v", p, err)
			}
		}
	case "apt":
	case "generic":
		if r.Generic == nil {
			return
		}
		switch r.Generic.DefaultClass {
		case "deny", "mutable":
		default:
			errs.add("%s.default_class: must be deny or mutable", p)
		}
		for j := range r.Generic.Rules {
			rule := &r.Generic.Rules[j]
			rp := fmt.Sprintf("%s.rules[%d]", p, j)
			if rule.Match == "" {
				errs.add("%s.match: required", rp)
			} else if _, err := regexp.Compile(rule.Match); err != nil {
				errs.add("%s.match: invalid regular expression: %v", rp, err)
			}
			switch rule.Class {
			case "immutable", "mutable", "deny":
			default:
				errs.add("%s.class: must be immutable, mutable or deny", rp)
			}
			if rule.FreshTTL != nil && *rule.FreshTTL < 0 {
				errs.add("%s.fresh_ttl: must not be negative", rp)
			}
		}
		for j := range r.Generic.Expect {
			e := &r.Generic.Expect[j]
			ep := fmt.Sprintf("%s.expect[%d]", p, j)
			if !strings.HasPrefix(e.Path, "/") {
				errs.add("%s.path: must start with '/'", ep)
			}
			if e.SHA256 == "" && e.SHA512 == "" && e.SHA1 == "" {
				errs.add("%s: at least one digest is required", ep)
			}
			validateDigest(ep+".sha256", e.SHA256, 64, errs)
			validateDigest(ep+".sha512", e.SHA512, 128, errs)
			validateDigest(ep+".sha1", e.SHA1, 40, errs)
		}
		for j := range r.Generic.Companion {
			comp := &r.Generic.Companion[j]
			cp := fmt.Sprintf("%s.companion[%d]", p, j)
			if comp.Suffix == "" {
				errs.add("%s.suffix: required", cp)
			} else if !strings.HasPrefix(comp.Suffix, ".") {
				errs.add("%s.suffix: must start with '.'", cp)
			}
			switch comp.Algo {
			case "sha256", "sha512", "sha1":
			default:
				errs.add("%s.algo: must be sha256, sha512 or sha1", cp)
			}
		}
	}
}

func validateTimeouts(path string, t Timeouts, errs *Errors) {
	values := []struct {
		name  string
		value Duration
	}{
		{"connect", t.Connect},
		{"tls_handshake", t.TLSHandshake},
		{"response_header", t.ResponseHeader},
		{"stall", t.Stall},
		{"revalidate_total", t.RevalidateTotal},
		{"wait_timeout", t.WaitTimeout},
		{"read_header_timeout", t.ReadHeaderTimeout},
		{"idle_timeout", t.IdleTimeout},
	}
	for _, v := range values {
		if v.value <= 0 {
			errs.add("%s.%s: must be greater than zero", path, v.name)
		}
	}
	if t.FetchTotal < 0 {
		errs.add("%s.fetch_total: must not be negative", path)
	}
}

func validateTimeoutOverrides(path string, o *TimeoutOverrides, errs *Errors) {
	values := []struct {
		name  string
		value *Duration
	}{
		{"connect", o.Connect},
		{"tls_handshake", o.TLSHandshake},
		{"response_header", o.ResponseHeader},
		{"stall", o.Stall},
		{"revalidate_total", o.RevalidateTotal},
		{"wait_timeout", o.WaitTimeout},
		{"read_header_timeout", o.ReadHeaderTimeout},
		{"idle_timeout", o.IdleTimeout},
	}
	for _, v := range values {
		if v.value != nil && *v.value <= 0 {
			errs.add("%s.%s: must be greater than zero", path, v.name)
		}
	}
	if o.FetchTotal != nil && *o.FetchTotal < 0 {
		errs.add("%s.fetch_total: must not be negative", path)
	}
}

func validateMount(mount string) error {
	if mount == "" {
		return fmt.Errorf("required")
	}
	if !strings.HasPrefix(mount, "/") {
		return fmt.Errorf("must start with '/'")
	}
	if mount != "/" && strings.HasSuffix(mount, "/") {
		return fmt.Errorf("must not end with '/'")
	}
	if path.Clean(mount) != mount {
		return fmt.Errorf("must be normalized")
	}
	for _, segment := range strings.Split(mount, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("must not contain dot segments")
		}
	}
	return nil
}

func mountsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return mountPrefix(a, b) || mountPrefix(b, a)
}

func mountPrefix(prefix, value string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
		return false
	}
	return prefix == "/" || value[len(prefix)] == '/'
}

func validateHTTPURL(field, value string, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("required")
	}
	u, err := url.Parse(value)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("host is required")
	}
	return nil
}

func validateHostname(value string) error {
	if value == "" {
		return fmt.Errorf("required")
	}
	if strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("must not contain '/' or '\\'")
	}
	u, err := url.Parse("http://" + value)
	if err != nil {
		return err
	}
	if u.Host != value || u.Path != "" || u.User != nil {
		return fmt.Errorf("invalid hostname %q", value)
	}
	return nil
}

func validateHostPort(value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("host is required")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func validateProxy(p Proxy) error {
	switch p {
	case ProxyDirect, ProxyEnv:
		return nil
	case "":
		return fmt.Errorf("must be direct, env or a proxy URL")
	}
	u, err := url.Parse(string(p))
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("scheme must be http, https, socks5 or socks5h")
	}
	if u.Host == "" {
		return fmt.Errorf("host is required")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("must not contain a path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must not contain a query or fragment")
	}
	return nil
}

func validateDigest(field, value string, length int, errs *Errors) {
	if value == "" {
		return
	}
	if len(value) != length {
		errs.add("%s: must be %d hex characters", field, length)
		return
	}
	if _, err := hex.DecodeString(value); err != nil {
		errs.add("%s: must be hexadecimal", field)
	}
}

func validateCAFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return fmt.Errorf("contains no PEM certificates")
	}
	return nil
}

func validateDataDir(dir string) error {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("not a directory")
		}
		if err := unix.Access(dir, unix.W_OK|unix.X_OK); err != nil {
			return fmt.Errorf("not writable: %v", err)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// validateListen checks a host:port address, so that a typo is reported by
// config check instead of failing when the listener is opened.
func validateListen(value string, optional bool) error {
	if value == "" {
		if optional {
			return nil
		}
		return fmt.Errorf("required")
	}
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%q is not a host:port address (%v)", value, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return fmt.Errorf("%q has an invalid port", value)
	}
	return nil
}
