# Scope and non-goals

## In scope

| Area | Detail |
|---|---|
| Ecosystems | Go modules and sumdb, Maven repository layout (Central, Google, Gradle plugin portal, any other Maven repo), Gradle wrapper distributions, npm, PyPI (HTML and JSON simple API), crates.io sparse index and crate downloads, rustup toolchains, apt (Debian, Ubuntu, any apt repo), generic URL caching |
| Upstream handling | Ordered upstream lists, failover, circuit breaker, HTTP and SOCKS5 proxies, per-upstream request headers, optional fixed connect address |
| Cache | Content-addressed blobs, SQLite index, generations of mutable metadata, negative cache, optional gzip at rest for text |
| Integrity | Digest verification from protocol metadata, stored `verified` state, scrub, quarantine |
| Storage management | Budget, watermarks, per-repo quotas, eviction policies, pin sets, emergency bypass, history pruning |
| Offline | Automatic stale fallback, explicit offline mode, fast failure |
| Tools | `warm` from lockfiles and local client caches, `seed` from a curated list, `ingest` from path-mirrored directories, export and import |
| Operations | Health, readiness, Prometheus metrics, JSON logs, admin API on a separate listener, Docker image, Traefik example |

## Out of scope

| Excluded | Reason |
|---|---|
| Docker/OCI registry | Token flow, manifest lists, blob redirects, and multi-registry mirroring make it a different product. |
| Publishing or private package hosting | Larder is read only. Use a real registry for private packages and point Larder at it as an upstream if needed. |
| Web UI | Operations use CLI, metrics, and logs. |
| Authentication and users | The service is meant for a private network or behind a reverse proxy. An optional CIDR allow list exists. |
| Dependency resolution | Larder caches what clients ask for. `warm` and `seed` use lockfiles or the real client tools to determine what to fetch. |
| Signature verification | Clients verify signatures (apt GPG, Go sumdb). Larder forwards signed material unchanged. |
| Cold storage tier | A second storage directory adds failure modes. Use export and import to move data to another disk. |
| Multi-node clustering | One node, one data directory. |
| Content transformation | No recompression of artifacts, no vulnerability scanning, no license scanning. |
| Windows and macOS as server targets | The server targets Linux. The CLI builds elsewhere but is not a supported server. |

## Compatibility promises

- Configuration files are versioned (`version: 1`); unknown keys are rejected with a clear error.
- The on-disk layout and index schema carry a schema version; migrations run at startup and are forward only.
- Command names and flags in [04-cli](../04-cli/index.md) are stable once released.
