package config

// The raw types mirror the YAML file. Optional scalars are pointers so the
// effective configuration can distinguish an omitted key from a zero value.

type rawConfig struct {
	Version           *int         `yaml:"version"`
	DataDir           *string      `yaml:"data_dir"`
	Listen            *string      `yaml:"listen"`
	PublicBaseURL     *string      `yaml:"public_base_url"`
	TrustForwarded    *bool        `yaml:"trust_forwarded"`
	Routing           *string      `yaml:"routing"`
	Offline           *bool        `yaml:"offline"`
	OfflineMissStatus *int         `yaml:"offline_miss_status"`
	UpstreamProxy     *string      `yaml:"upstream_proxy"`
	MaxHeaderBytes    *string      `yaml:"max_header_bytes"`
	ShutdownGrace     *string      `yaml:"shutdown_grace"`
	Admin             *rawAdmin    `yaml:"admin"`
	Access            *rawAccess   `yaml:"access"`
	Log               *rawLog      `yaml:"log"`
	Timeouts          *rawTimeouts `yaml:"timeouts"`
	Limits            *rawLimits   `yaml:"limits"`
	Breaker           *rawBreaker  `yaml:"breaker"`
	Cache             *rawCache    `yaml:"cache"`
	Storage           *rawStorage  `yaml:"storage"`
	Repos             []rawRepo    `yaml:"repos"`
}

type rawAdmin struct {
	Listen *string `yaml:"listen"`
}

type rawAccess struct {
	AllowCIDRs []string `yaml:"allow_cidrs"`
}

type rawLog struct {
	Level      *string  `yaml:"level"`
	Format     *string  `yaml:"format"`
	SampleHits *float64 `yaml:"sample_hits"`
}

type rawTimeouts struct {
	Connect           *string `yaml:"connect"`
	TLSHandshake      *string `yaml:"tls_handshake"`
	ResponseHeader    *string `yaml:"response_header"`
	Stall             *string `yaml:"stall"`
	RevalidateTotal   *string `yaml:"revalidate_total"`
	FetchTotal        *string `yaml:"fetch_total"`
	WaitTimeout       *string `yaml:"wait_timeout"`
	ReadHeaderTimeout *string `yaml:"read_header_timeout"`
	IdleTimeout       *string `yaml:"idle_timeout"`
}

type rawLimits struct {
	MaxObjectSize           *string `yaml:"max_object_size"`
	BackgroundCompleteMax   *string `yaml:"background_complete_max"`
	MaxConcurrentFetches    *int    `yaml:"max_concurrent_fetches"`
	MaxUpstreamConnsPerHost *int    `yaml:"max_upstream_conns_per_host"`
	MaxParseBytes           *string `yaml:"max_parse_bytes"`
	BlockPrivateRedirects   *bool   `yaml:"block_private_redirects"`
}

type rawBreaker struct {
	FailureThreshold *int    `yaml:"failure_threshold"`
	OpenFor          *string `yaml:"open_for"`
	OpenMax          *string `yaml:"open_max"`
}

type rawCache struct {
	CompressMetadata     *bool   `yaml:"compress_metadata"`
	MetadataHistory      *int    `yaml:"metadata_history"`
	NegativeTTL          *string `yaml:"negative_ttl"`
	ServeGone            *bool   `yaml:"serve_gone"`
	StaleWhileRevalidate *string `yaml:"stale_while_revalidate"`
	MaxStale             *string `yaml:"max_stale"`
}

type rawStorage struct {
	MaxBytes           *string      `yaml:"max_bytes"`
	MinFreeBytes       *string      `yaml:"min_free_bytes"`
	HardFreeBytes      *string      `yaml:"hard_free_bytes"`
	HighWatermark      *float64     `yaml:"high_watermark"`
	LowWatermark       *float64     `yaml:"low_watermark"`
	GCInterval         *string      `yaml:"gc_interval"`
	GCMaxDuration      *string      `yaml:"gc_max_duration"`
	EvictionPolicy     *string      `yaml:"eviction_policy"`
	ProtectRecentDays  *int         `yaml:"protect_recent_days"`
	MetadataUnusedDays *int         `yaml:"metadata_unused_days"`
	QuarantineKeepDays *int         `yaml:"quarantine_keep_days"`
	SnapshotKeep       *int         `yaml:"snapshot_keep"`
	Scrub              *rawScrub    `yaml:"scrub"`
	Protect            []rawProtect `yaml:"protect"`
}

type rawScrub struct {
	DailyPercent *int `yaml:"daily_percent"`
	RateMBs      *int `yaml:"rate_mb_s"`
}

type rawProtect struct {
	Repo  string `yaml:"repo"`
	Match string `yaml:"match"`
}

type rawRepo struct {
	Name              *string       `yaml:"name"`
	Kind              *string       `yaml:"kind"`
	Mount             *string       `yaml:"mount"`
	Host              *string       `yaml:"host"`
	PublicURL         *string       `yaml:"public_url"`
	Upstreams         []rawUpstream `yaml:"upstreams"`
	Proxy             *string       `yaml:"proxy"`
	FreshTTL          *string       `yaml:"fresh_ttl"`
	RevalidateTimeout *string       `yaml:"revalidate_timeout"`
	NegativeTTL       *string       `yaml:"negative_ttl"`
	ServeGone         *bool         `yaml:"serve_gone"`
	MaxObjectSize     *string       `yaml:"max_object_size"`
	QuotaBytes        *string       `yaml:"quota_bytes"`
	EvictWeight       *float64      `yaml:"evict_weight"`
	UnusedDays        *int          `yaml:"unused_days"`
	RequireVerified   *bool         `yaml:"require_verified"`
	Protect           []string      `yaml:"protect"`

	Verify           *string               `yaml:"verify"`
	SumDBUpstreams   map[string][]string   `yaml:"sumdb_upstreams"`
	CompanionFetch   *bool                 `yaml:"companion_fetch"`
	Audit            *string               `yaml:"audit"`
	AuditTimeout     *string               `yaml:"audit_timeout"`
	FilesHosts       *[]string             `yaml:"files_hosts"`
	DLUpstreams      *[]string             `yaml:"dl_upstreams"`
	SuiteConsistency *bool                 `yaml:"suite_consistency"`
	Rules            []rawGenericRule      `yaml:"rules"`
	DefaultClass     *string               `yaml:"default_class"`
	KeepQuery        *bool                 `yaml:"keep_query"`
	RejectHTML       *bool                 `yaml:"reject_html"`
	Expect           []rawGenericExpect    `yaml:"expect"`
	Companion        []rawGenericCompanion `yaml:"companion"`
}

type rawUpstream struct {
	URL       string               `yaml:"url"`
	Proxy     *string              `yaml:"proxy"`
	Headers   map[string]string    `yaml:"headers"`
	ConnectTo string               `yaml:"connect_to"`
	CAFile    *string              `yaml:"ca_file"`
	Timeouts  *rawTimeoutOverrides `yaml:"timeouts"`
}

type rawTimeoutOverrides struct {
	Connect           *string `yaml:"connect"`
	TLSHandshake      *string `yaml:"tls_handshake"`
	ResponseHeader    *string `yaml:"response_header"`
	Stall             *string `yaml:"stall"`
	RevalidateTotal   *string `yaml:"revalidate_total"`
	FetchTotal        *string `yaml:"fetch_total"`
	WaitTimeout       *string `yaml:"wait_timeout"`
	ReadHeaderTimeout *string `yaml:"read_header_timeout"`
	IdleTimeout       *string `yaml:"idle_timeout"`
}

type rawGenericRule struct {
	Match    string  `yaml:"match"`
	Class    string  `yaml:"class"`
	FreshTTL *string `yaml:"fresh_ttl"`
}

type rawGenericExpect struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
	SHA512 string `yaml:"sha512"`
	SHA1   string `yaml:"sha1"`
}

type rawGenericCompanion struct {
	Suffix string `yaml:"suffix"`
	Algo   string `yaml:"algo"`
}
