package config

import "time"

// Defaults returns the effective configuration with every documented default.
func Defaults() *Config {
	return &Config{
		Version:           1,
		DataDir:           "./data",
		Listen:            "127.0.0.1:8080",
		PublicBaseURL:     "",
		TrustForwarded:    false,
		Routing:           RoutingPath,
		Offline:           false,
		OfflineMissStatus: 504,
		UpstreamProxy:     ProxyDirect,
		MaxHeaderBytes:    Size(64 << 10),
		ShutdownGrace:     Duration(30 * time.Second),
		Admin: Admin{
			Listen: "127.0.0.1:9090",
		},
		Access: Access{
			AllowCIDRs: []string{},
		},
		Log: Log{
			Level:      "info",
			Format:     "json",
			SampleHits: 1,
		},
		Timeouts: Timeouts{
			Connect:           Duration(5 * time.Second),
			TLSHandshake:      Duration(5 * time.Second),
			ResponseHeader:    Duration(15 * time.Second),
			Stall:             Duration(30 * time.Second),
			RevalidateTotal:   Duration(6 * time.Second),
			FetchTotal:        0,
			WaitTimeout:       Duration(15 * time.Minute),
			ReadHeaderTimeout: Duration(10 * time.Second),
			IdleTimeout:       Duration(120 * time.Second),
		},
		Limits: Limits{
			MaxObjectSize:           Size(2 << 30),
			BackgroundCompleteMax:   Size(512 << 20),
			MaxConcurrentFetches:    32,
			MaxUpstreamConnsPerHost: 8,
			MaxParseBytes:           Size(512 << 20),
			BlockPrivateRedirects:   true,
		},
		Breaker: Breaker{
			FailureThreshold: 3,
			OpenFor:          Duration(30 * time.Second),
			OpenMax:          Duration(5 * time.Minute),
		},
		Cache: Cache{
			CompressMetadata:     true,
			MetadataHistory:      2,
			NegativeTTL:          Duration(60 * time.Second),
			ServeGone:            true,
			StaleWhileRevalidate: 0,
			MaxStale:             0,
		},
		Storage: Storage{
			MaxBytes:           0,
			MinFreeBytes:       Size(5 << 30),
			HardFreeBytes:      Size(1 << 30),
			HighWatermark:      0.90,
			LowWatermark:       0.75,
			GCInterval:         Duration(5 * time.Minute),
			GCMaxDuration:      Duration(5 * time.Minute),
			EvictionPolicy:     "lru",
			ProtectRecentDays:  14,
			MetadataUnusedDays: 365,
			QuarantineKeepDays: 30,
			SnapshotKeep:       7,
			Scrub: Scrub{
				DailyPercent: 2,
				RateMBs:      20,
			},
			Protect: []StorageProtect{},
		},
		Repos: []Repo{},
	}
}

func defaultFreshTTL(kind string) Duration {
	switch kind {
	case "go", "npm", "pypi", "crates":
		return Duration(60 * time.Second)
	case "maven", "rustup", "apt", "alpine", "generic":
		return Duration(300 * time.Second)
	default:
		return 0
	}
}

func defaultCratesDLUpstreams() []string {
	return []string{"https://static.crates.io/crates/{crate}/{crate}-{version}.crate"}
}
