# Observability

## Logging

Structured JSON through `log/slog` on stdout. Levels: `debug`, `info`, `warn`, `error` (`log.level`). Optional `log.format: text` for local use.

Per-request line (info), fields:

`ts`, `level`, `msg="request"`, `repo`, `method`, `path`, `status`, `bytes`, `duration_ms`, `cache` (value of `X-Larder-Cache`), `class`, `upstream`, `stale`, `verified`, `client` (peer IP), `request_id`.

Other events (named in `msg`): `fetch_start`, `fetch_done`, `fetch_failed`, `upstream_circuit_open`, `upstream_circuit_closed`, `digest_conflict`, `verify_failed`, `gc_start`, `gc_done`, `bypass_start`, `bypass_end`, `scrub_corrupt`, `config_reloaded`, `offline_changed`.

Request logging can be sampled for hits with `log.sample_hits` (default 1.0 means log all).

## Metrics

Prometheus text format on `/metrics` (admin listener). Implemented by hand in a small package; no client library is required.

| Metric | Type | Labels |
|---|---|---|
| `larder_requests_total` | counter | `repo`, `class`, `result` (`hit`,`miss`,`revalidated`,`stale`,`bypass`,`offline_hit`,`offline_miss`,`gone`,`error`), `status` |
| `larder_request_duration_seconds` | histogram | `repo`, `class` |
| `larder_bytes_served_total` | counter | `repo` |
| `larder_bytes_fetched_total` | counter | `repo`, `upstream` |
| `larder_upstream_requests_total` | counter | `repo`, `upstream`, `outcome` (`ok`,`not_found`,`error`,`timeout`,`invalid`) |
| `larder_upstream_up` | gauge | `upstream` (1 closed circuit, 0 open) |
| `larder_inflight_fetches` | gauge | |
| `larder_cache_objects` | gauge | `repo`, `class` |
| `larder_cache_bytes` | gauge | `repo`, `class` |
| `larder_pinned_bytes` | gauge | |
| `larder_budget_bytes` | gauge | |
| `larder_used_bytes` | gauge | |
| `larder_disk_free_bytes` | gauge | |
| `larder_pinned_over_budget` | gauge | |
| `larder_bypass_active` | gauge | |
| `larder_offline` | gauge | |
| `larder_stale_served_total` | counter | `repo` |
| `larder_verify_failures_total` | counter | `repo` |
| `larder_scrub_corrupt_total` | counter | |
| `larder_gc_runs_total` | counter | |
| `larder_gc_evicted_bytes_total` | counter | `tier` |
| `larder_gc_evicted_objects_total` | counter | `tier` |
| `larder_build_info` | gauge | `version`, `go_version` |

Suggested alerts: `larder_upstream_up == 0` for 10 m, `larder_used_bytes / larder_budget_bytes > 0.95`, `larder_bypass_active == 1`, `rate(larder_scrub_corrupt_total[1d]) > 0`, `larder_pinned_over_budget == 1`.

## Health endpoints

| Path | Meaning |
|---|---|
| `/healthz` | process is up (200 always when the handler runs) |
| `/readyz` | database reachable, data directory writable, disk above hard floor; 503 otherwise with a JSON reason list |

## Response headers

See [request lifecycle](request-lifecycle.md). Every response includes `X-Larder-Cache`, which makes `curl -I` the quickest debugging tool.

## Request IDs

Each request gets an ID (`X-Request-Id` accepted from a trusted proxy or generated) logged and returned in the response.
