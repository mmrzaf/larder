package config

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

var defaultComments = map[string]string{
	"version":                            "Configuration format version; must be 1.",
	"data_dir":                           "Data directory; all cache files live under this path on one filesystem.",
	"listen":                             "Public listener address.",
	"public_base_url":                    "Public base URL for URL rewriting; empty derives it from each request.",
	"trust_forwarded":                    "Trust X-Forwarded-* headers from a reverse proxy.",
	"routing":                            "Repo selection mode: path, host or both.",
	"offline":                            "Start in offline mode.",
	"offline_miss_status":                "Status returned for a miss while offline: 404, 502, 503 or 504.",
	"upstream_proxy":                     "Default outbound proxy: direct, env, or an http, https, socks5 or socks5h URL.",
	"max_header_bytes":                   "Maximum request header size.",
	"shutdown_grace":                     "Graceful shutdown wait.",
	"admin":                              "Administrative listener settings.",
	"admin.listen":                       "Admin listener address; empty disables the admin listener.",
	"access":                             "Public listener access control.",
	"access.allow_cidrs":                 "Allowed client networks; empty allows every address that can reach the port.",
	"log":                                "Logging settings.",
	"log.level":                          "debug, info, warn or error.",
	"log.format":                         "json or text.",
	"log.sample_hits":                    "Fraction of cache-hit request lines to log.",
	"timeouts":                           "Global upstream and server timeouts.",
	"timeouts.connect":                   "TCP or proxy connection timeout.",
	"timeouts.tls_handshake":             "TLS handshake timeout.",
	"timeouts.response_header":           "Time allowed before response headers arrive.",
	"timeouts.stall":                     "Maximum time without receiving body bytes.",
	"timeouts.revalidate_total":          "Total time allowed for one mutable revalidation.",
	"timeouts.wait_timeout":              "Maximum time a request waits for a concurrent fetch.",
	"timeouts.read_header_timeout":       "HTTP server request-header read timeout.",
	"timeouts.idle_timeout":              "HTTP server idle connection timeout.",
	"limits":                             "Size and concurrency limits.",
	"limits.max_object_size":             "Objects larger than this are streamed without being stored.",
	"limits.background_complete_max":     "Downloads up to this size finish after the client disconnects.",
	"limits.max_concurrent_fetches":      "Global cap on concurrent upstream fetches.",
	"limits.max_upstream_conns_per_host": "Cap on upstream connections per host.",
	"limits.max_parse_bytes":             "Maximum decompressed size used for parsing and rewriting.",
	"limits.block_private_redirects":     "Refuse upstream redirects to loopback, link-local and private addresses.",
	"breaker":                            "Circuit breaker settings.",
	"breaker.failure_threshold":          "Consecutive failures that open an upstream circuit.",
	"breaker.open_for":                   "Initial open period.",
	"breaker.open_max":                   "Maximum open period after repeated failures.",
	"cache":                              "Cache policy settings.",
	"cache.compress_metadata":            "Gzip text objects at rest.",
	"cache.metadata_history":             "Previous generations kept per mutable key.",
	"cache.negative_ttl":                 "In-memory lifetime of a negative result.",
	"cache.serve_gone":                   "Keep serving a stored object after upstream reports 404 or 410.",
	"cache.stale_while_revalidate":       "Serve stale objects within this window and refresh in the background.",
	"cache.max_stale":                    "Maximum stale age that may be served; 0 means unlimited.",
	"storage":                            "Storage budget and eviction settings.",
	"storage.max_bytes":                  "Budget cap; 0 means only the filesystem limit applies.",
	"storage.min_free_bytes":             "Free space kept available on the filesystem.",
	"storage.hard_free_bytes":            "Free-space threshold that triggers emergency bypass.",
	"storage.high_watermark":             "Usage fraction that starts GC.",
	"storage.low_watermark":              "Usage fraction at which GC stops.",
	"storage.gc_interval":                "Usage check period.",
	"storage.gc_max_duration":            "Maximum duration of one GC run.",
	"storage.eviction_policy":            "lru, lfu or score.",
	"storage.protect_recent_days":        "Recent-access boundary between eviction tiers 3 and 4.",
	"storage.metadata_unused_days":       "Age at which unused current mutable objects enter tier 5.",
	"storage.quarantine_keep_days":       "Retention for quarantined blobs.",
	"storage.snapshot_keep":              "Index snapshots to retain.",
	"storage.scrub":                      "Background scrub settings.",
	"storage.scrub.daily_percent":        "Share of blobs verified per day.",
	"storage.scrub.rate_mb_s":            "Scrub read throttle in MiB per second.",
	"storage.protect":                    "Declarative pins: {repo, match} pairs.",
	"repos": "Repositories. Add one entry per repository.\n" +
		"Common keys and defaults:\n" +
		"  name required; kind required (go, maven, npm, pypi, crates, rustup, apt, generic)\n" +
		"  mount /<name>; host none; public_url none\n" +
		"  upstreams required (rustup defaults to https://static.rust-lang.org)\n" +
		"  proxy inherits upstream_proxy; headers none; connect_to none; ca_file none; timeouts inherit global\n" +
		"  fresh_ttl 60s for go, npm, pypi and crates; 300s for maven, rustup, apt and generic\n" +
		"  revalidate_timeout inherits timeouts.revalidate_total; negative_ttl inherits cache.negative_ttl\n" +
		"  serve_gone inherits cache.serve_gone; max_object_size inherits limits.max_object_size\n" +
		"  quota_bytes 0; evict_weight 1.0; unused_days 0; require_verified false; protect []\n" +
		"Kind keys and defaults:\n" +
		"  go: verify basic; sumdb_upstreams derived\n" +
		"  maven: companion_fetch true\n" +
		"  npm: audit passthrough; audit_timeout 3s\n" +
		"  pypi: files_hosts [files.pythonhosted.org]\n" +
		"  crates: dl_upstreams [https://static.crates.io/crates/{crate}/{crate}-{version}.crate]\n" +
		"  apt: suite_consistency true\n" +
		"  generic: rules []; default_class deny; keep_query false; reject_html true; expect []; companion []",
}

// DefaultYAML returns a complete annotated configuration containing every default.
func DefaultYAML() ([]byte, error) {
	data, err := yaml.Marshal(Defaults())
	if err != nil {
		return nil, fmt.Errorf("marshal default configuration: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse default configuration: %w", err)
	}
	if len(doc.Content) > 0 {
		annotateNode(doc.Content[0], "", defaultComments)
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode default configuration: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close default configuration encoder: %w", err)
	}
	return out.Bytes(), nil
}

func annotateNode(node *yaml.Node, path string, comments map[string]string) {
	if node == nil {
		return
	}
	if node.Kind == yaml.DocumentNode {
		for _, child := range node.Content {
			annotateNode(child, path, comments)
		}
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			value := node.Content[i+1]
			p := joinPath(path, key.Value)
			if comment, ok := comments[p]; ok {
				key.HeadComment = comment
			}
			annotateNode(value, p, comments)
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			annotateNode(child, path, comments)
		}
	}
}
