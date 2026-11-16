# Troubleshooting

First step for any problem: `curl -sI <url>` and read the `X-Larder-*` headers, then `larder objects <repo> <key>`, then the log line for the request (`jq 'select(.path=="/npm/lodash")'`).

| Symptom | Likely cause | What to do |
|---|---|---|
| 504 with `OFFLINE-MISS` | Larder is offline (or the object was never cached) | `larder offline status`; warm the missing item when online |
| 502 from an upstream-reachable server | every upstream failed or verification failed | check the log for `verify_failed` and `upstream_circuit_open`; `larder config check --connect` |
| Requests slow when upstreams are down | circuits not yet open, or large `revalidate_timeout` | lower `timeouts.revalidate_total`; circuits open after 3 failures |
| `X-Larder-Stale: 1` on everything | upstream unreachable | check `/_larder/v1/upstreams` and outbound proxy |
| npm `EINTEGRITY` | upstream re-published a version, or a stored tarball differs from the new packument | log has `digest_conflict`; `larder objects npm <tarball key>`; evict the object and refetch if the new bytes are intended |
| npm installs fetch from the registry host directly | packument not rewritten: client got a cached packument through another path, or `public_base_url` is wrong | check `dist.tarball` in the served packument; set `public_base_url` or `trust_forwarded` |
| pip hash mismatch | stored file differs from lockfile hash | compare with `larder objects`; the file may have been re-uploaded upstream |
| pip cannot find files | the index links point outside allowed hosts | check `files_hosts` and the upstream host list |
| apt `Hash Sum mismatch` | index files stored from different mirror syncs | `larder evict apt-debian 'dists/bookworm/**'` and `apt-get update`; ensure `suite_consistency: true`; use by-hash |
| apt `Release file is expired` | stale `Release` past `Valid-Until` while offline | client setting `Acquire::Check-Valid-Until "false"` |
| Go `SECURITY ERROR` checksum mismatch | go.sum disagrees with the content | do not bypass; compare `larder objects go <zip key>`; check `GOFLAGS` and `GONOSUMDB` |
| Go `verifying module: ... unexpected status` for sumdb | sumdb not reachable through any configured sumdb upstream | configure `sumdb_upstreams` |
| cargo `failed to load source ... config.json` | wrong registry URL or missing trailing slash | URL must be `sparse+https://host/crates/index/` |
| rustup cannot find manifests | `RUSTUP_DIST_SERVER` mount wrong | request `https://host/rustup/dist/channel-rust-stable.toml` with curl |
| 403 on requests | `access.allow_cidrs` | add the client network, or check `trust_forwarded` and the forwarded address |
| 404 for a valid path | path not allowed by the kind (generic default deny) | add a rule |
| Disk usage above budget | GC cannot reach the low watermark (pins) | see [storage runbook](storage-runbook.md) |
| `larder_bypass_active 1` | disk below hard floor | run `larder gc`, free space |
| Startup refuses: index corrupt | database damaged | restore the latest snapshot ([backup](backup-and-recovery.md)) |
| Startup refuses: lock held | another process uses the data directory | stop it, or check for a stale container |
| Memory growth | very large metadata documents parsed repeatedly | check `larder_request_duration_seconds` for the repo; report with the package name; parsing is streamed and bounded |
| Downloads stall at the same byte count | reverse proxy timeout or buffering | adjust the proxy's response timeout |
| Corrupt blob detected | disk error | `larder verify --repair`; check the disk's SMART status |
| Log `discarded_broken_object` | a stored blob was missing or had the wrong size; the entry was dropped and fetched again | check the disk and any process that touches `data_dir`; the damaged file is in `quarantine/` |
| Log `response_aborted`, client reports a reset or "premature close" | the upstream failed after the response had started, or storing failed while a client was being served | expected on upstream failures; the client retries and nothing was cached from the failed transfer |
| 500 or 507 from a miss, log `fetch_local_failure` | writing to `data_dir` failed (full disk, quota, inodes, permissions) | free space or fix permissions; the upstream is not at fault and its circuit is not charged |
| 500 `stored object failed its size check` | the blob could not be read (permissions or device error); the entry is kept because the data may be fine | fix permissions on `data_dir/blobs`; check the device |
| Log `admin_listener_not_loopback` | admin API bound to a non-loopback address; it has no authentication | bind it to `127.0.0.1` or a private network |
| `npm audit` fails with 503 | the advisory service is unreachable or rejected the request | expected while the upstream is down; Larder never answers an audit with an invented clean result (unless `audit: stub`) |

## Debug mode

`larder serve -v` (or `log.level: debug`) logs each upstream attempt with status and timing. Do not leave it on in production for busy servers.

## Reporting issues

Include: `larder version`, the redacted output of `larder config print`, the failing request's headers (`curl -sI`), and the matching log lines.
