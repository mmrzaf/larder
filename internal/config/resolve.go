package config

import (
	"fmt"
	"os"
)

func resolve(raw *rawConfig) (*Config, Errors) {
	var errs Errors
	c := Defaults()
	if raw.Version != nil {
		c.Version = *raw.Version
	} else {
		c.Version = 0
	}
	c.DataDir = stringOr(raw.DataDir, c.DataDir)
	c.Listen = stringOr(raw.Listen, c.Listen)
	c.PublicBaseURL = stringOr(raw.PublicBaseURL, c.PublicBaseURL)
	c.TrustForwarded = boolOr(raw.TrustForwarded, c.TrustForwarded)
	if raw.Routing != nil {
		c.Routing = Routing(*raw.Routing)
	}
	c.Offline = boolOr(raw.Offline, c.Offline)
	c.OfflineMissStatus = intOr(raw.OfflineMissStatus, c.OfflineMissStatus)
	c.UpstreamProxy = Proxy(stringOr(raw.UpstreamProxy, string(c.UpstreamProxy)))
	c.MaxHeaderBytes = sizeValue("max_header_bytes", raw.MaxHeaderBytes, c.MaxHeaderBytes, &errs)
	c.ShutdownGrace = durationValue("shutdown_grace", raw.ShutdownGrace, c.ShutdownGrace, &errs)

	if raw.Admin != nil {
		c.Admin.Listen = stringOr(raw.Admin.Listen, c.Admin.Listen)
	}
	if raw.Access != nil {
		c.Access.AllowCIDRs = cloneStrings(raw.Access.AllowCIDRs)
	}
	if raw.Log != nil {
		c.Log.Level = stringOr(raw.Log.Level, c.Log.Level)
		c.Log.Format = stringOr(raw.Log.Format, c.Log.Format)
		c.Log.SampleHits = floatOr(raw.Log.SampleHits, c.Log.SampleHits)
	}
	if raw.Timeouts != nil {
		c.Timeouts.Connect = durationValue("timeouts.connect", raw.Timeouts.Connect, c.Timeouts.Connect, &errs)
		c.Timeouts.TLSHandshake = durationValue("timeouts.tls_handshake", raw.Timeouts.TLSHandshake, c.Timeouts.TLSHandshake, &errs)
		c.Timeouts.ResponseHeader = durationValue("timeouts.response_header", raw.Timeouts.ResponseHeader, c.Timeouts.ResponseHeader, &errs)
		c.Timeouts.Stall = durationValue("timeouts.stall", raw.Timeouts.Stall, c.Timeouts.Stall, &errs)
		c.Timeouts.RevalidateTotal = durationValue("timeouts.revalidate_total", raw.Timeouts.RevalidateTotal, c.Timeouts.RevalidateTotal, &errs)
		c.Timeouts.FetchTotal = durationValue("timeouts.fetch_total", raw.Timeouts.FetchTotal, c.Timeouts.FetchTotal, &errs)
		c.Timeouts.WaitTimeout = durationValue("timeouts.wait_timeout", raw.Timeouts.WaitTimeout, c.Timeouts.WaitTimeout, &errs)
		c.Timeouts.ReadHeaderTimeout = durationValue("timeouts.read_header_timeout", raw.Timeouts.ReadHeaderTimeout, c.Timeouts.ReadHeaderTimeout, &errs)
		c.Timeouts.IdleTimeout = durationValue("timeouts.idle_timeout", raw.Timeouts.IdleTimeout, c.Timeouts.IdleTimeout, &errs)
	}
	if raw.Limits != nil {
		c.Limits.MaxObjectSize = sizeValue("limits.max_object_size", raw.Limits.MaxObjectSize, c.Limits.MaxObjectSize, &errs)
		c.Limits.BackgroundCompleteMax = sizeValue("limits.background_complete_max", raw.Limits.BackgroundCompleteMax, c.Limits.BackgroundCompleteMax, &errs)
		c.Limits.MaxConcurrentFetches = intOr(raw.Limits.MaxConcurrentFetches, c.Limits.MaxConcurrentFetches)
		c.Limits.MaxUpstreamConnsPerHost = intOr(raw.Limits.MaxUpstreamConnsPerHost, c.Limits.MaxUpstreamConnsPerHost)
		c.Limits.MaxParseBytes = sizeValue("limits.max_parse_bytes", raw.Limits.MaxParseBytes, c.Limits.MaxParseBytes, &errs)
		c.Limits.BlockPrivateRedirects = boolOr(raw.Limits.BlockPrivateRedirects, c.Limits.BlockPrivateRedirects)
	}
	if raw.Breaker != nil {
		c.Breaker.FailureThreshold = intOr(raw.Breaker.FailureThreshold, c.Breaker.FailureThreshold)
		c.Breaker.OpenFor = durationValue("breaker.open_for", raw.Breaker.OpenFor, c.Breaker.OpenFor, &errs)
		c.Breaker.OpenMax = durationValue("breaker.open_max", raw.Breaker.OpenMax, c.Breaker.OpenMax, &errs)
	}
	if raw.Cache != nil {
		c.Cache.CompressMetadata = boolOr(raw.Cache.CompressMetadata, c.Cache.CompressMetadata)
		c.Cache.MetadataHistory = intOr(raw.Cache.MetadataHistory, c.Cache.MetadataHistory)
		c.Cache.NegativeTTL = durationValue("cache.negative_ttl", raw.Cache.NegativeTTL, c.Cache.NegativeTTL, &errs)
		c.Cache.ServeGone = boolOr(raw.Cache.ServeGone, c.Cache.ServeGone)
		c.Cache.StaleWhileRevalidate = durationValue("cache.stale_while_revalidate", raw.Cache.StaleWhileRevalidate, c.Cache.StaleWhileRevalidate, &errs)
		c.Cache.MaxStale = durationValue("cache.max_stale", raw.Cache.MaxStale, c.Cache.MaxStale, &errs)
	}
	if raw.Storage != nil {
		c.Storage.MaxBytes = sizeValue("storage.max_bytes", raw.Storage.MaxBytes, c.Storage.MaxBytes, &errs)
		c.Storage.MinFreeBytes = sizeValue("storage.min_free_bytes", raw.Storage.MinFreeBytes, c.Storage.MinFreeBytes, &errs)
		c.Storage.HardFreeBytes = sizeValue("storage.hard_free_bytes", raw.Storage.HardFreeBytes, c.Storage.HardFreeBytes, &errs)
		c.Storage.HighWatermark = floatOr(raw.Storage.HighWatermark, c.Storage.HighWatermark)
		c.Storage.LowWatermark = floatOr(raw.Storage.LowWatermark, c.Storage.LowWatermark)
		c.Storage.GCInterval = durationValue("storage.gc_interval", raw.Storage.GCInterval, c.Storage.GCInterval, &errs)
		c.Storage.GCMaxDuration = durationValue("storage.gc_max_duration", raw.Storage.GCMaxDuration, c.Storage.GCMaxDuration, &errs)
		c.Storage.EvictionPolicy = stringOr(raw.Storage.EvictionPolicy, c.Storage.EvictionPolicy)
		c.Storage.ProtectRecentDays = intOr(raw.Storage.ProtectRecentDays, c.Storage.ProtectRecentDays)
		c.Storage.MetadataUnusedDays = intOr(raw.Storage.MetadataUnusedDays, c.Storage.MetadataUnusedDays)
		c.Storage.QuarantineKeepDays = intOr(raw.Storage.QuarantineKeepDays, c.Storage.QuarantineKeepDays)
		c.Storage.SnapshotKeep = intOr(raw.Storage.SnapshotKeep, c.Storage.SnapshotKeep)
		if raw.Storage.Scrub != nil {
			c.Storage.Scrub.DailyPercent = intOr(raw.Storage.Scrub.DailyPercent, c.Storage.Scrub.DailyPercent)
			c.Storage.Scrub.RateMBs = intOr(raw.Storage.Scrub.RateMBs, c.Storage.Scrub.RateMBs)
		}
		c.Storage.Protect = make([]StorageProtect, 0, len(raw.Storage.Protect))
		for _, p := range raw.Storage.Protect {
			c.Storage.Protect = append(c.Storage.Protect, StorageProtect{Repo: p.Repo, Match: p.Match})
		}
	}

	c.Repos = make([]Repo, 0, len(raw.Repos))
	for i := range raw.Repos {
		c.Repos = append(c.Repos, resolveRepo(&raw.Repos[i], fmt.Sprintf("repos[%d]", i), c, &errs))
	}
	return c, errs
}

func resolveRepo(raw *rawRepo, path string, c *Config, errs *Errors) Repo {
	name := stringOr(raw.Name, "")
	kind := stringOr(raw.Kind, "")
	r := Repo{
		Name:              name,
		Kind:              kind,
		Mount:             stringOr(raw.Mount, ""),
		Host:              stringOr(raw.Host, ""),
		PublicURL:         stringOr(raw.PublicURL, ""),
		Proxy:             Proxy(stringOr(raw.Proxy, string(c.UpstreamProxy))),
		FreshTTL:          durationValue(path+".fresh_ttl", raw.FreshTTL, defaultFreshTTL(kind), errs),
		RevalidateTimeout: durationValue(path+".revalidate_timeout", raw.RevalidateTimeout, c.Timeouts.RevalidateTotal, errs),
		NegativeTTL:       durationValue(path+".negative_ttl", raw.NegativeTTL, c.Cache.NegativeTTL, errs),
		ServeGone:         boolOr(raw.ServeGone, c.Cache.ServeGone),
		MaxObjectSize:     sizeValue(path+".max_object_size", raw.MaxObjectSize, c.Limits.MaxObjectSize, errs),
		MaxParseBytes:     c.Limits.MaxParseBytes,
		QuotaBytes:        sizeValue(path+".quota_bytes", raw.QuotaBytes, 0, errs),
		EvictWeight:       floatOr(raw.EvictWeight, 1.0),
		UnusedDays:        intOr(raw.UnusedDays, 0),
		RequireVerified:   boolOr(raw.RequireVerified, false),
		Protect:           cloneStrings(raw.Protect),
	}
	if r.Mount == "" && name != "" {
		r.Mount = "/" + name
	}

	r.Upstreams = make([]Upstream, 0, len(raw.Upstreams))
	for j := range raw.Upstreams {
		r.Upstreams = append(r.Upstreams, resolveUpstream(raw.Upstreams[j], fmt.Sprintf("%s.upstreams[%d]", path, j), r.Proxy, errs))
	}
	if kind == "rustup" && len(r.Upstreams) == 0 {
		r.Upstreams = append(r.Upstreams, Upstream{
			URL:   "https://static.rust-lang.org",
			Proxy: r.Proxy,
		})
	}

	switch kind {
	case "go":
		r.Go = &GoOptions{Verify: stringOr(raw.Verify, "basic")}
		if raw.SumDBUpstreams != nil {
			r.Go.SumDBUpstreams = cloneStringMap(raw.SumDBUpstreams)
		}
	case "maven":
		r.Maven = &MavenOptions{CompanionFetch: boolOr(raw.CompanionFetch, true)}
	case "npm":
		r.NPM = &NPMOptions{
			Audit:        stringOr(raw.Audit, "passthrough"),
			AuditTimeout: durationValue(path+".audit_timeout", raw.AuditTimeout, Duration(3_000_000_000), errs),
		}
	case "pypi":
		if raw.FilesHosts != nil {
			r.PyPI = &PyPIOptions{FilesHosts: cloneStrings(*raw.FilesHosts)}
		} else {
			r.PyPI = &PyPIOptions{FilesHosts: []string{"files.pythonhosted.org"}}
		}
	case "crates":
		if raw.DLUpstreams != nil {
			r.Crates = &CratesOptions{DLUpstreams: cloneStrings(*raw.DLUpstreams)}
		} else {
			r.Crates = &CratesOptions{DLUpstreams: defaultCratesDLUpstreams()}
		}
	case "apt":
		r.APT = &APTOptions{SuiteConsistency: boolOr(raw.SuiteConsistency, true)}
	case "generic":
		g := &GenericOptions{
			DefaultClass: stringOr(raw.DefaultClass, "deny"),
			KeepQuery:    boolOr(raw.KeepQuery, false),
			RejectHTML:   boolOr(raw.RejectHTML, true),
			Rules:        make([]GenericRule, 0, len(raw.Rules)),
			Expect:       make([]GenericExpect, 0, len(raw.Expect)),
			Companion:    make([]GenericCompanion, 0, len(raw.Companion)),
		}
		for j, rule := range raw.Rules {
			gr := GenericRule{Match: rule.Match, Class: rule.Class}
			if rule.FreshTTL != nil {
				d := durationValue(fmt.Sprintf("%s.rules[%d].fresh_ttl", path, j), rule.FreshTTL, 0, errs)
				gr.FreshTTL = &d
			}
			g.Rules = append(g.Rules, gr)
		}
		for _, e := range raw.Expect {
			g.Expect = append(g.Expect, GenericExpect(e))
		}
		for _, comp := range raw.Companion {
			g.Companion = append(g.Companion, GenericCompanion(comp))
		}
		r.Generic = g
	}
	return r
}

func resolveUpstream(raw rawUpstream, path string, repoProxy Proxy, errs *Errors) Upstream {
	proxy := repoProxy
	if raw.Proxy != nil {
		proxy = Proxy(*raw.Proxy)
	}
	u := Upstream{
		URL:       raw.URL,
		Proxy:     proxy,
		Headers:   cloneStringMap(raw.Headers),
		ConnectTo: raw.ConnectTo,
		CAFile:    stringOr(raw.CAFile, ""),
	}
	if raw.Timeouts != nil {
		ov := &TimeoutOverrides{}
		if raw.Timeouts.Connect != nil {
			d := durationValue(path+".timeouts.connect", raw.Timeouts.Connect, 0, errs)
			ov.Connect = &d
		}
		if raw.Timeouts.TLSHandshake != nil {
			d := durationValue(path+".timeouts.tls_handshake", raw.Timeouts.TLSHandshake, 0, errs)
			ov.TLSHandshake = &d
		}
		if raw.Timeouts.ResponseHeader != nil {
			d := durationValue(path+".timeouts.response_header", raw.Timeouts.ResponseHeader, 0, errs)
			ov.ResponseHeader = &d
		}
		if raw.Timeouts.Stall != nil {
			d := durationValue(path+".timeouts.stall", raw.Timeouts.Stall, 0, errs)
			ov.Stall = &d
		}
		if raw.Timeouts.RevalidateTotal != nil {
			d := durationValue(path+".timeouts.revalidate_total", raw.Timeouts.RevalidateTotal, 0, errs)
			ov.RevalidateTotal = &d
		}
		if raw.Timeouts.FetchTotal != nil {
			d := durationValue(path+".timeouts.fetch_total", raw.Timeouts.FetchTotal, 0, errs)
			ov.FetchTotal = &d
		}
		if raw.Timeouts.WaitTimeout != nil {
			d := durationValue(path+".timeouts.wait_timeout", raw.Timeouts.WaitTimeout, 0, errs)
			ov.WaitTimeout = &d
		}
		if raw.Timeouts.ReadHeaderTimeout != nil {
			d := durationValue(path+".timeouts.read_header_timeout", raw.Timeouts.ReadHeaderTimeout, 0, errs)
			ov.ReadHeaderTimeout = &d
		}
		if raw.Timeouts.IdleTimeout != nil {
			d := durationValue(path+".timeouts.idle_timeout", raw.Timeouts.IdleTimeout, 0, errs)
			ov.IdleTimeout = &d
		}
		if ov.hasValues() {
			u.Timeouts = ov
		}
	}
	return u
}

func (o *TimeoutOverrides) hasValues() bool {
	return o.Connect != nil || o.TLSHandshake != nil || o.ResponseHeader != nil || o.Stall != nil ||
		o.RevalidateTotal != nil || o.FetchTotal != nil || o.WaitTimeout != nil || o.ReadHeaderTimeout != nil || o.IdleTimeout != nil
}

func sizeValue(path string, raw *string, def Size, errs *Errors) Size {
	if raw == nil {
		return def
	}
	v, err := ParseSize(*raw)
	if err != nil {
		errs.add("%s: %v", path, err)
		return def
	}
	return v
}

func durationValue(path string, raw *string, def Duration, errs *Errors) Duration {
	if raw == nil {
		return def
	}
	v, err := ParseDuration(*raw)
	if err != nil {
		errs.add("%s: %v", path, err)
		return def
	}
	return v
}

func stringOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func floatOr(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneStringMap[V any](in map[string]V) map[string]V {
	if in == nil {
		return nil
	}
	out := make(map[string]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func applyEnvOverrides(c *Config) {
	if v, ok := os.LookupEnv("LARDER_DATA_DIR"); ok {
		c.DataDir = v
	}
	if v, ok := os.LookupEnv("LARDER_LISTEN"); ok {
		c.Listen = v
	}
	if v, ok := os.LookupEnv("LARDER_ADMIN_LISTEN"); ok {
		c.Admin.Listen = v
	}
	if v, ok := os.LookupEnv("LARDER_LOG_LEVEL"); ok {
		c.Log.Level = v
	}
}
