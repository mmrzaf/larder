# 02 Architecture

| File | Contents |
|---|---|
| [overview.md](overview.md) | Components, data flow, process model |
| [request-lifecycle.md](request-lifecycle.md) | Step-by-step handling of a request, response headers, status codes |
| [cache-model.md](cache-model.md) | Classes, keys, variants, TTLs, generations, negative cache |
| [storage-layout.md](storage-layout.md) | Directory structure, blob writes, crash safety, compression at rest |
| [index-schema.md](index-schema.md) | SQLite schema and access patterns |
| [upstreams.md](upstreams.md) | Upstream selection, failover, circuit breaker, proxies, timeouts |
| [offline-and-staleness.md](offline-and-staleness.md) | Offline mode, stale serving, `serve_gone`, negative results |
| [integrity.md](integrity.md) | Expectations, verification, scrub, quarantine |
| [storage-management.md](storage-management.md) | Budget, watermarks, quotas, eviction tiers and policies, pins, emergency bypass |
| [concurrency-and-failure.md](concurrency-and-failure.md) | Coalescing, detached downloads, limits, failure behaviour |
| [security.md](security.md) | Threat model and mitigations |
| [observability.md](observability.md) | Logs, metrics, health endpoints, response headers |
