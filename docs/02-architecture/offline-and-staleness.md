# Offline and staleness

## Three situations

| Situation | Detection | Behaviour |
|---|---|---|
| Upstream slow or briefly failing | request-level timeout or error | Serve stale for mutable, fall through to next upstream for misses. |
| Upstream down for a while | circuit breaker open on all upstreams | Requests fail in milliseconds instead of waiting for timeouts; stale still served. |
| Operator declares offline | `offline: true` in config, or `larder offline on` | No upstream contact at all. |

## Offline mode

- Set with `offline: true` in configuration, at runtime with `larder offline on|off`, or via `POST /_larder/v1/offline`.
- A runtime switch lasts until the process restarts; the configured value applies at start.
- In offline mode every mutable hit is served as stale (`X-Larder-Cache: OFFLINE-HIT`, `X-Larder-Stale: 1`), every miss returns `offline_miss_status` (default 504) with a plain-text body naming the object and the reason.
- Metrics expose `larder_offline 1`.

## Why 504 for an offline miss

The status is configurable (`offline_miss_status`: 404, 502, 503, 504). Defaults and client behaviour:

| Client | Behaviour on 404 | Behaviour on 5xx |
|---|---|---|
| Go | with `GOPROXY=...,direct` falls through on 404/410 only | with `,` stops; with `|` falls through |
| npm, pnpm, yarn | treats as missing package | retries with back-off (set `fetch-retries=0`) |
| pip | treats as missing | retries (set `--retries 0`) |
| cargo | treats as missing | retries |
| apt | error either way | error either way |

504 is accurate and avoids clients silently concluding that a package does not exist. Use 404 if you prefer tools to fall through to `direct` or another repository quickly. Client settings that make offline failures immediate are listed in [client setup](../05-configuration/client-setup.md).

## Stale serving rules

1. A stale response always carries `X-Larder-Stale: 1` and `X-Larder-Age`.
2. `max_stale` (default unlimited) caps how old a stale object may be.
3. Stale objects are never served in place of a *different* variant.
4. apt: stale `Release` files may be past `Valid-Until`; apt clients must be configured with `Acquire::Check-Valid-Until "false"` (see [apt](../03-ecosystems/apt.md)). Larder never edits signed files.

## Gone upstream objects

When upstream returns 404 or 410 for a key Larder holds, the stored object is served (`serve_gone: true`) with `X-Larder-Cache: GONE` and `X-Larder-Upstream-Status: 404`. This protects against unpublished packages and mirrors that drop old versions. For mutable objects a 404 never creates a new generation.

## Recovery

When upstreams return, the first request for a stale object revalidates it. If upstream content changed in a way that conflicts with stored artifacts (for example a re-published npm version with a different `integrity`), the new packument is stored as a new generation, and the previously stored tarball is retained under its original key. Expectation mismatch handling is described in [integrity](integrity.md).
