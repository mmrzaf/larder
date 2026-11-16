package config

import "net/url"

// Routing is the repository selection mode.
type Routing string

const (
	// RoutingPath selects repos by URL path prefix.
	RoutingPath Routing = "path"
	// RoutingHost selects repos by request Host.
	RoutingHost Routing = "host"
	// RoutingBoth accepts either a matching host or a matching path.
	RoutingBoth Routing = "both"
)

// Proxy is an outbound proxy setting.
type Proxy string

const (
	// ProxyDirect connects without a proxy.
	ProxyDirect Proxy = "direct"
	// ProxyEnv uses the standard proxy environment variables.
	ProxyEnv Proxy = "env"
)

// String returns the configured proxy text.
func (p Proxy) String() string { return string(p) }

// URL parses a proxy URL. It returns nil for direct and env modes.
func (p Proxy) URL() (*url.URL, error) {
	if p == ProxyDirect || p == ProxyEnv || p == "" {
		return nil, nil
	}
	return url.Parse(string(p))
}

// Config is the effective, validated configuration.
type Config struct {
	Version           int      `yaml:"version"`
	DataDir           string   `yaml:"data_dir"`
	Listen            string   `yaml:"listen"`
	PublicBaseURL     string   `yaml:"public_base_url"`
	TrustForwarded    bool     `yaml:"trust_forwarded"`
	Routing           Routing  `yaml:"routing"`
	Offline           bool     `yaml:"offline"`
	OfflineMissStatus int      `yaml:"offline_miss_status"`
	UpstreamProxy     Proxy    `yaml:"upstream_proxy"`
	MaxHeaderBytes    Size     `yaml:"max_header_bytes"`
	ShutdownGrace     Duration `yaml:"shutdown_grace"`
	Admin             Admin    `yaml:"admin"`
	Access            Access   `yaml:"access"`
	Log               Log      `yaml:"log"`
	Timeouts          Timeouts `yaml:"timeouts"`
	Limits            Limits   `yaml:"limits"`
	Breaker           Breaker  `yaml:"breaker"`
	Cache             Cache    `yaml:"cache"`
	Storage           Storage  `yaml:"storage"`
	Repos             []Repo   `yaml:"repos"`
}

// Admin holds the administrative listener settings.
type Admin struct {
	Listen string `yaml:"listen"`
}

// Access holds the public listener access list.
type Access struct {
	AllowCIDRs []string `yaml:"allow_cidrs"`
}

// Log holds logging settings.
type Log struct {
	Level      string  `yaml:"level"`
	Format     string  `yaml:"format"`
	SampleHits float64 `yaml:"sample_hits"`
}

// Timeouts holds global upstream and server timeouts.
type Timeouts struct {
	Connect           Duration `yaml:"connect"`
	TLSHandshake      Duration `yaml:"tls_handshake"`
	ResponseHeader    Duration `yaml:"response_header"`
	Stall             Duration `yaml:"stall"`
	RevalidateTotal   Duration `yaml:"revalidate_total"`
	FetchTotal        Duration `yaml:"fetch_total"`
	WaitTimeout       Duration `yaml:"wait_timeout"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
}

// Limits holds size and concurrency limits.
type Limits struct {
	MaxObjectSize           Size `yaml:"max_object_size"`
	BackgroundCompleteMax   Size `yaml:"background_complete_max"`
	MaxConcurrentFetches    int  `yaml:"max_concurrent_fetches"`
	MaxUpstreamConnsPerHost int  `yaml:"max_upstream_conns_per_host"`
	MaxParseBytes           Size `yaml:"max_parse_bytes"`
	BlockPrivateRedirects   bool `yaml:"block_private_redirects"`
}

// Breaker holds circuit breaker settings.
type Breaker struct {
	FailureThreshold int      `yaml:"failure_threshold"`
	OpenFor          Duration `yaml:"open_for"`
	OpenMax          Duration `yaml:"open_max"`
}

// Cache holds cache policy settings.
type Cache struct {
	CompressMetadata     bool     `yaml:"compress_metadata"`
	MetadataHistory      int      `yaml:"metadata_history"`
	NegativeTTL          Duration `yaml:"negative_ttl"`
	ServeGone            bool     `yaml:"serve_gone"`
	StaleWhileRevalidate Duration `yaml:"stale_while_revalidate"`
	MaxStale             Duration `yaml:"max_stale"`
}

// Storage holds budget and eviction settings.
type Storage struct {
	MaxBytes           Size             `yaml:"max_bytes"`
	MinFreeBytes       Size             `yaml:"min_free_bytes"`
	HardFreeBytes      Size             `yaml:"hard_free_bytes"`
	HighWatermark      float64          `yaml:"high_watermark"`
	LowWatermark       float64          `yaml:"low_watermark"`
	GCInterval         Duration         `yaml:"gc_interval"`
	GCMaxDuration      Duration         `yaml:"gc_max_duration"`
	EvictionPolicy     string           `yaml:"eviction_policy"`
	ProtectRecentDays  int              `yaml:"protect_recent_days"`
	MetadataUnusedDays int              `yaml:"metadata_unused_days"`
	QuarantineKeepDays int              `yaml:"quarantine_keep_days"`
	SnapshotKeep       int              `yaml:"snapshot_keep"`
	Scrub              Scrub            `yaml:"scrub"`
	Protect            []StorageProtect `yaml:"protect"`
}

// Scrub holds background scrub settings.
type Scrub struct {
	DailyPercent int `yaml:"daily_percent"`
	RateMBs      int `yaml:"rate_mb_s"`
}

// StorageProtect is a declarative key glob that is always pinned.
type StorageProtect struct {
	Repo  string `yaml:"repo"`
	Match string `yaml:"match"`
}

// Repo is the effective configuration of one repository.
type Repo struct {
	Name              string
	Kind              string
	Mount             string
	Host              string
	PublicURL         string
	Upstreams         []Upstream
	Proxy             Proxy
	FreshTTL          Duration
	RevalidateTimeout Duration
	NegativeTTL       Duration
	ServeGone         bool
	MaxObjectSize     Size
	QuotaBytes        Size
	EvictWeight       float64
	UnusedDays        int
	RequireVerified   bool
	MaxParseBytes     Size `yaml:"-"`
	Protect           []string

	Go      *GoOptions
	Maven   *MavenOptions
	NPM     *NPMOptions
	PyPI    *PyPIOptions
	Crates  *CratesOptions
	APT     *APTOptions
	Generic *GenericOptions
}

// Upstream is the effective configuration of one upstream entry.
type Upstream struct {
	URL       string            `yaml:"url"`
	Proxy     Proxy             `yaml:"proxy,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	ConnectTo string            `yaml:"connect_to,omitempty"`
	CAFile    string            `yaml:"ca_file,omitempty"`
	Timeouts  *TimeoutOverrides `yaml:"timeouts,omitempty"`
}

// TimeoutOverrides holds per-upstream timeout overrides.
type TimeoutOverrides struct {
	Connect           *Duration `yaml:"connect,omitempty"`
	TLSHandshake      *Duration `yaml:"tls_handshake,omitempty"`
	ResponseHeader    *Duration `yaml:"response_header,omitempty"`
	Stall             *Duration `yaml:"stall,omitempty"`
	RevalidateTotal   *Duration `yaml:"revalidate_total,omitempty"`
	FetchTotal        *Duration `yaml:"fetch_total,omitempty"`
	WaitTimeout       *Duration `yaml:"wait_timeout,omitempty"`
	ReadHeaderTimeout *Duration `yaml:"read_header_timeout,omitempty"`
	IdleTimeout       *Duration `yaml:"idle_timeout,omitempty"`
}

// Apply overlays non-nil overrides onto the base timeouts.
func (o *TimeoutOverrides) Apply(base Timeouts) Timeouts {
	if o == nil {
		return base
	}
	if o.Connect != nil {
		base.Connect = *o.Connect
	}
	if o.TLSHandshake != nil {
		base.TLSHandshake = *o.TLSHandshake
	}
	if o.ResponseHeader != nil {
		base.ResponseHeader = *o.ResponseHeader
	}
	if o.Stall != nil {
		base.Stall = *o.Stall
	}
	if o.RevalidateTotal != nil {
		base.RevalidateTotal = *o.RevalidateTotal
	}
	if o.FetchTotal != nil {
		base.FetchTotal = *o.FetchTotal
	}
	if o.WaitTimeout != nil {
		base.WaitTimeout = *o.WaitTimeout
	}
	if o.ReadHeaderTimeout != nil {
		base.ReadHeaderTimeout = *o.ReadHeaderTimeout
	}
	if o.IdleTimeout != nil {
		base.IdleTimeout = *o.IdleTimeout
	}
	return base
}

// GoOptions holds settings for the go kind.
type GoOptions struct {
	Verify         string              `yaml:"verify"`
	SumDBUpstreams map[string][]string `yaml:"sumdb_upstreams,omitempty"`
}

// MavenOptions holds settings for the maven kind.
type MavenOptions struct {
	CompanionFetch bool `yaml:"companion_fetch"`
}

// NPMOptions holds settings for the npm kind.
type NPMOptions struct {
	Audit        string   `yaml:"audit"`
	AuditTimeout Duration `yaml:"audit_timeout"`
}

// PyPIOptions holds settings for the pypi kind.
type PyPIOptions struct {
	FilesHosts []string `yaml:"files_hosts"`
}

// CratesOptions holds settings for the crates kind.
type CratesOptions struct {
	DLUpstreams []string `yaml:"dl_upstreams"`
}

// APTOptions holds settings for the apt kind.
type APTOptions struct {
	SuiteConsistency bool `yaml:"suite_consistency"`
}

// GenericOptions holds settings for the generic kind.
type GenericOptions struct {
	Rules        []GenericRule      `yaml:"rules"`
	DefaultClass string             `yaml:"default_class"`
	KeepQuery    bool               `yaml:"keep_query"`
	RejectHTML   bool               `yaml:"reject_html"`
	Expect       []GenericExpect    `yaml:"expect"`
	Companion    []GenericCompanion `yaml:"companion"`
}

// GenericRule classifies paths for a generic repo. Rules are evaluated first to last.
type GenericRule struct {
	Match    string    `yaml:"match"`
	Class    string    `yaml:"class"`
	FreshTTL *Duration `yaml:"fresh_ttl,omitempty"`
}

// GenericExpect declares a known digest for an exact generic path.
type GenericExpect struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256,omitempty"`
	SHA512 string `yaml:"sha512,omitempty"`
	SHA1   string `yaml:"sha1,omitempty"`
}

// GenericCompanion declares a digest file fetched next to an artifact.
type GenericCompanion struct {
	Suffix string `yaml:"suffix"`
	Algo   string `yaml:"algo"`
}

// MarshalYAML renders the repository with only the keys that apply to its kind.
func (r Repo) MarshalYAML() (any, error) {
	out := map[string]any{
		"name":               r.Name,
		"kind":               r.Kind,
		"mount":              r.Mount,
		"upstreams":          r.Upstreams,
		"proxy":              r.Proxy,
		"fresh_ttl":          r.FreshTTL,
		"revalidate_timeout": r.RevalidateTimeout,
		"negative_ttl":       r.NegativeTTL,
		"serve_gone":         r.ServeGone,
		"max_object_size":    r.MaxObjectSize,
		"quota_bytes":        r.QuotaBytes,
		"evict_weight":       r.EvictWeight,
		"unused_days":        r.UnusedDays,
		"require_verified":   r.RequireVerified,
		"protect":            r.Protect,
	}
	if r.Host != "" {
		out["host"] = r.Host
	}
	if r.PublicURL != "" {
		out["public_url"] = r.PublicURL
	}

	switch r.Kind {
	case "go":
		if r.Go != nil {
			out["verify"] = r.Go.Verify
			if len(r.Go.SumDBUpstreams) > 0 {
				out["sumdb_upstreams"] = r.Go.SumDBUpstreams
			}
		}
	case "maven":
		if r.Maven != nil {
			out["companion_fetch"] = r.Maven.CompanionFetch
		}
	case "npm":
		if r.NPM != nil {
			out["audit"] = r.NPM.Audit
			out["audit_timeout"] = r.NPM.AuditTimeout
		}
	case "pypi":
		if r.PyPI != nil {
			out["files_hosts"] = r.PyPI.FilesHosts
		}
	case "crates":
		if r.Crates != nil {
			out["dl_upstreams"] = r.Crates.DLUpstreams
		}
	case "apt":
		if r.APT != nil {
			out["suite_consistency"] = r.APT.SuiteConsistency
		}
	case "generic":
		if r.Generic != nil {
			out["rules"] = r.Generic.Rules
			out["default_class"] = r.Generic.DefaultClass
			out["keep_query"] = r.Generic.KeepQuery
			out["reject_html"] = r.Generic.RejectHTML
			out["expect"] = r.Generic.Expect
			out["companion"] = r.Generic.Companion
		}
	}
	return out, nil
}
