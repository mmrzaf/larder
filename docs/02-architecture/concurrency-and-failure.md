# Concurrency and failure behaviour

## Singleflight per key

At most one fetch per `(repo, key)` runs at a time. Other requests for the same key become waiters.

- The first requester for an immutable miss receives bytes as they arrive (tee from the download into the response) when a single upstream is tried and no digest expectation applies. When verification or upstream failover is required, the first requester waits for the commit and then reads from disk like the others. `wait_timeout` (default 15 m) bounds the wait; on timeout a waiter gets 504.
- For mutable misses everyone waits for the commit, then reads from disk (documents are small).
- If the fetch fails, all waiters receive the same failure.

## Detached downloads

The download is not tied to the first client's connection. If the client disconnects:

- objects up to `background_complete_max` (default 512 MiB) keep downloading to completion so the work is not wasted;
- larger objects are aborted to avoid spending bandwidth nobody wants.

## Limits

| Setting | Default | Purpose |
|---|---|---|
| `max_concurrent_fetches` | 32 | global cap on upstream downloads in flight |
| `max_upstream_conns_per_host` | 8 | per-host connection cap |
| `max_parse_bytes` | 512 MiB | decompression and parse caps |
| `read_header_timeout` | 10 s | public listener |
| `idle_timeout` | 120 s | public listener |

Slow clients do not block fetches because the temp file is the source of truth for waiters.

## Failure behaviour

| Failure | Result |
|---|---|
| Upstream stalls mid-body | The fetch fails after `stall`; the temp file is removed; the next upstream is tried from the start. |
| Upstream dies after the first byte reached a client (streamed miss, passthrough, bypass) | The client connection is aborted rather than ended cleanly, so a chunked response cannot pass for a complete one. Nothing is stored. |
| Upstream dies mid-body for a verified object | Nothing has been sent yet: 502 (or the next upstream is tried). Nothing is stored. |
| Process killed mid-download | Temp files are removed at the next start. Blob files are flushed and renamed before any index row refers to them, so no entry can point at an incomplete file. |
| Disk full or storage error while writing | The candidate is discarded. When no client is being served, the request answers 500 (507 when the filesystem reports no space). When a client is already receiving the body, the download is completed for it and only the caching fails. The upstream circuit is not charged for local failures. |
| Download larger than the free space | Admitted only if the free space, minus the bytes already promised to downloads in progress, stays above `hard_free_bytes`; otherwise streamed with `BYPASS` and not stored. |
| Stored blob missing or its size differs from the index | The entry is dropped (the file is quarantined), the request is handled as a miss and fetched again. Offline, the request fails; a damaged blob is never served. |
| Blob sweep (garbage collection, reconcile) during a download | Publication of a blob and removal of unreferenced blobs exclude each other, so a sweep cannot remove a blob that is about to be referenced. Garbage collection never removes pinned or referenced content. |
| Index database locked | Retried with `busy_timeout`; persistent failure marks `/readyz` unhealthy but already-open serving continues from disk where possible. |
| Panic in a handler | Recovered, logged with stack trace, request answered 500; process continues. |
| Config error on reload (SIGHUP) | Old configuration remains active; error logged. |

Retries are bounded: each request tries every configured upstream at most once, and an upstream whose circuit is open is skipped immediately. Clients own the retry policy (npm, apt and pip retry by themselves); Larder does not multiply their attempts.

## Graceful shutdown

On SIGTERM or SIGINT: stop accepting connections, wait up to `shutdown_grace` (default 30 s) for requests, flush access times, close the database, release the lock. In-flight detached downloads are cancelled and their temp files removed.

## Reload

SIGHUP re-reads the configuration. Applied without restart: repos, upstreams, timeouts, TTLs, storage limits, offline default, access list, logging level. Listener addresses and `data_dir` require a restart.
