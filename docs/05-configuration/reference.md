# Configuration reference

## Top level

| Key | Type | Default | Meaning |
|---|---|---|---|
| `version` | int | required | must be `1` |
| `data_dir` | path | `./data` | data directory (one filesystem) |
| `listen` | addr | `127.0.0.1:8080` | public listener |
| `public_base_url` | url | empty | base used when rewriting URLs (`https://larder.example`); empty means derive from the request |
| `trust_forwarded` | bool | `false` | trust `X-Forwarded-*` headers |
| `routing` | `path`\|`host`\|`both` | `path` | how repos are selected |
| `offline` | bool | `false` | start in offline mode |
| `offline_miss_status` | int | `504` | status for a miss while offline (404, 502, 503, 504) |
| `upstream_proxy` | string | `direct` | default outbound proxy: `direct`, `env`, `http://...`, `socks5://...`, `socks5h://...` |
| `max_header_bytes` | size | `64KiB` | request header limit |
| `shutdown_grace` | duration | `30s` | graceful shutdown wait |

## `admin`

| Key | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:9090` | admin listener (health, metrics, admin API); empty disables it |

## `access`

| Key | Default | Meaning |
|---|---|---|
| `allow_cidrs` | `[]` | allowed client networks on the public listener; empty allows all |

## `log`

| Key | Default | Meaning |
|---|---|---|
| `level` | `info` | `debug`, `info`, `warn`, `error` |
| `format` | `json` | `json` or `text` |
| `sample_hits` | `1.0` | fraction of cache-hit request lines logged |

## `timeouts`

| Key | Default |
|---|---|
| `connect` | `5s` |
| `tls_handshake` | `5s` |
| `response_header` | `15s` |
| `stall` | `30s` |
| `revalidate_total` | `6s` |
| `fetch_total` | `0` (none) |
| `wait_timeout` | `15m` |
| `read_header_timeout` | `10s` |
| `idle_timeout` | `120s` |

## `limits`

| Key | Default | Meaning |
|---|---|---|
| `max_object_size` | `2GiB` | larger objects are streamed, not stored |
| `background_complete_max` | `512MiB` | downloads up to this size finish after client disconnect |
| `max_concurrent_fetches` | `32` | global |
| `max_upstream_conns_per_host` | `8` | per host |
| `max_parse_bytes` | `512MiB` | decompression and parse cap |
| `block_private_redirects` | `true` | refuse upstream redirects to loopback, link-local, and private addresses |

## `breaker`

| Key | Default |
|---|---|
| `failure_threshold` | `3` |
| `open_for` | `30s` |
| `open_max` | `5m` |

## `cache`

| Key | Default | Meaning |
|---|---|---|
| `compress_metadata` | `true` | gzip text objects at rest |
| `metadata_history` | `2` | previous generations kept per mutable key |
| `negative_ttl` | `60s` | in-memory 404 memory |
| `serve_gone` | `true` | keep serving stored objects that upstream now reports 404 or 410 |
| `stale_while_revalidate` | `0s` | background refresh window (0 off) |
| `max_stale` | `0` | maximum stale age; 0 means unlimited |

## `storage`

| Key | Default | Meaning |
|---|---|---|
| `max_bytes` | `0` | budget cap (0 means only the disk limit applies) |
| `min_free_bytes` | `5GiB` | keep this much free on the filesystem |
| `hard_free_bytes` | `1GiB` | emergency bypass threshold |
| `high_watermark` | `0.90` | start GC |
| `low_watermark` | `0.75` | stop GC |
| `gc_interval` | `5m` | usage check |
| `gc_max_duration` | `5m` | per run |
| `eviction_policy` | `lru` | `lru`, `lfu`, `score` |
| `protect_recent_days` | `14` | tier 3 versus tier 4 boundary |
| `metadata_unused_days` | `365` | tier 5 threshold |
| `quarantine_keep_days` | `30` | |
| `snapshot_keep` | `7` | |
| `scrub.daily_percent` | `2` | share of blobs verified per day |
| `scrub.rate_mb_s` | `20` | read throttle |
| `protect` | `[]` | list of `{repo, match}` declarative pins |

## `repos[]`

Common keys:

| Key | Default | Meaning |
|---|---|---|
| `name` | required | unique; letters, digits, `.`, `_`, `-` |
| `kind` | required | `go`, `maven`, `npm`, `pypi`, `crates`, `rustup`, `apt`, `alpine`, `generic` |
| `mount` | `/<name>` | path prefix in path routing; must not overlap other mounts |
| `host` | none | hostname for host routing (`npm.mirror.example`) |
| `public_url` | none | full public URL of this repo, used when rewriting (overrides the global base) |
| `upstreams` | required | list of upstreams (below) |
| `proxy` | inherits | repo default outbound proxy |
| `fresh_ttl` | kind default | mutable freshness |
| `revalidate_timeout` | `timeouts.revalidate_total` | |
| `negative_ttl` | `cache.negative_ttl` | |
| `serve_gone` | `cache.serve_gone` | |
| `max_object_size` | `limits.max_object_size` | |
| `quota_bytes` | `0` | soft cap for immutable bytes |
| `evict_weight` | `1.0` | used by the `score` policy |
| `unused_days` | `0` | remove immutable objects unused this long; 0 off |
| `require_verified` | `false` | reject immutable objects that could not be verified |
| `protect` | `[]` | key globs always pinned |

Upstream entry: `url`, `proxy`, `headers` (map), `connect_to`, `ca_file`, `timeouts` (any of the timeout keys). `ca_file` adds PEM certificates to the system roots for that upstream.

Kind-specific keys:

| Kind | Key | Default | Meaning |
|---|---|---|---|
| go | `verify` | `basic` | `basic` or `strict` (sumdb `h1:` check) |
| go | `sumdb_upstreams` | derived | map of sumdb name to base URL list |
| maven | `companion_fetch` | `true` | fetch checksum files to verify artifacts |
| npm | `audit` | `passthrough` | `passthrough`, `stub`, `deny` |
| npm | `audit_timeout` | `3s` | |
| pypi | `files_hosts` | `[files.pythonhosted.org]` | extra allowed file hosts |
| crates | `dl_upstreams` | official static host | templates with `{crate}` and `{version}` |
| rustup | none | | upstream defaults to `https://static.rust-lang.org` |
| apt | `suite_consistency` | `true` | revalidate dependent index files when `Release` changes |
| generic | `rules`, `default_class`, `keep_query`, `reject_html`, `expect`, `companion` | see [generic](../03-ecosystems/generic.md) |

Generic sub-keys:

| Key | Type | Meaning |
|---|---|---|
| `rules[].match` | string | RE2 regular expression on the path after the mount |
| `rules[].class` | string | `immutable`, `mutable` or `deny` |
| `rules[].fresh_ttl` | duration | optional freshness override for a mutable rule |
| `expect[].path` | string | exact path the digest applies to |
| `expect[].sha256` | string | expected SHA-256 hex digest |
| `expect[].sha512` | string | expected SHA-512 hex digest |
| `expect[].sha1` | string | expected SHA-1 hex digest |
| `companion[].suffix` | string | suffix of the adjacent digest file, for example `.sha256` |
| `companion[].algo` | string | digest algorithm of the companion: `sha256`, `sha512` or `sha1` |

Declarative storage pins use one entry per rule with `protect[].repo` and `protect[].match`; `match` is a key glob.

## Validation rules

- Repo names unique; mounts unique and non-overlapping (no mount a prefix of another in path routing, unless separated at a segment boundary, for example `/maven/central` and `/maven/google` are fine, `/maven` and `/maven/central` are not).
- `low_watermark < high_watermark <= 1`.
- `hard_free_bytes <= min_free_bytes`.
- Host routing requires `host` on every repo; path routing requires `mount`.
- `offline_miss_status` must be one of 404, 502, 503, 504.
- Regular expressions compile (RE2).
- Every upstream URL is `http` or `https` with a host.
