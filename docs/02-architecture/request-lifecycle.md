# Request lifecycle

## Steps

1. **Receive.** Only `GET` and `HEAD` are accepted (plus `POST` on the npm audit endpoints, see [npm](../03-ecosystems/npm.md)). Anything else returns 405.
2. **Access check.** If `access.allow_cidrs` is set, the peer address (or the trusted forwarded address) must match, else 403.
3. **Route.** Longest-prefix match of the request path against repo mounts; in host mode, match the `Host` header first. Unknown mount returns 404.
4. **Normalize path.** Reject `..` segments, NUL bytes, backslashes, and double-encoded traversal with 400. Collapse duplicate slashes. Keep percent-encoding exactly as received (npm scoped names use `%2f`).
5. **Classify.** `kind.Classify` returns a `Plan`:
   - `Class`: immutable, mutable, passthrough, deny
   - `Key` and `Variant`
   - `UpstreamPath` (what to append to each upstream base)
   - `FreshTTL` override
   - `Rewrite` function (applied when serving), `Ingest` hook (run after commit), `Expect` function (digest lookup before commit)
   - or a synthesized response (for example the crates `config.json`)
   `deny` returns 404.
6. **Lookup** in the index by `(repo, key+variant)`.
7. **Decide** by class and state (see below).
8. **Serve** with the headers listed below.

## Decision table

| Class | State | Offline | Action |
|---|---|---|---|
| immutable | hit | any | serve blob |
| immutable | miss | no | fetch, store, serve |
| immutable | miss | yes | 504 `offline-miss` |
| mutable | hit, fresh | any | serve |
| mutable | hit, stale | no | revalidate (short timeout); 304: touch and serve; 200: new generation and serve; failure: serve stale |
| mutable | hit, stale | yes | serve stale |
| mutable | miss | no | fetch, store, serve; failure: 502/504 |
| mutable | miss | yes | 504 `offline-miss` |
| passthrough | n/a | no | stream; failure 502 |
| passthrough | n/a | yes | kind-specific stub or 504 |

## Fetching (miss or revalidation)

1. Acquire the per-key singleflight slot (see [concurrency](concurrency-and-failure.md)).
2. Walk the ordered upstream list, skipping upstreams whose circuit is open (unless all are open, then try them all once).
3. Send the request: method `GET`, `Accept` as required by the kind, `Accept-Encoding: gzip`, user agent `larder/<version>`, upstream-specific headers, conditional headers for revalidation. Follow up to 5 redirects. No `Range`.
4. Interpret the response:
   - 200: stream the body to a temp file while hashing; for the first requester, also tee to the client (immutable only).
   - 304: revalidation succeeded.
   - 404/410: try the next upstream; if all say so, the object does not exist (or is gone, see `serve_gone`).
   - 3xx beyond the limit, 4xx other than 404/410, 5xx, network error: record upstream failure and try the next.
5. Validate: status 200, `Content-Length` equals bytes received (when present), content type is not `text/html` for binary classes, size under `max_object_size`, digest matches an expectation if one exists. On failure discard the temp file, count the failure, try the next upstream.
6. Commit: fsync the temp file, rename to the blob path (if absent), insert or update rows in one transaction, run the `Ingest` hook (which may add expectations), and release waiters.

## Response headers

| Header | Values |
|---|---|
| `X-Larder-Cache` | `HIT`, `MISS`, `REVALIDATED`, `STALE`, `BYPASS`, `OFFLINE-HIT`, `OFFLINE-MISS`, `GONE` |
| `X-Larder-Stale` | `1` when a mutable object was served without successful revalidation |
| `X-Larder-Age` | seconds since last successful validation |
| `X-Larder-Upstream` | host of the upstream used for this response (misses and revalidations) |
| `X-Larder-Upstream-Status` | upstream status when it differs from 200, such as 404 with `GONE` |
| `X-Larder-Verified` | `1` when the digest matched an expectation, `0` otherwise |
| `X-Larder-Key` | `<repo>:<key>` |
| `Cache-Control` | immutable: `public, max-age=31536000, immutable`; mutable: `public, max-age=0, must-revalidate` |
| `ETag`, `Last-Modified` | taken from the stored object (rewritten responses use a derived ETag) |
| `Accept-Ranges` | `bytes` for blobs served from disk |

`Range` requests are served from disk for stored objects. A range request on a miss fetches the whole object first (no partial caching), then serves the range.

## Status codes returned by Larder

| Code | When |
|---|---|
| 200, 206, 304 | success, range, conditional match |
| 400 | invalid path |
| 403 | access check failed, or upstream host not allowed |
| 404 | unknown mount, denied path, or object does not exist upstream |
| 405 | method not allowed |
| 416 | unsatisfiable range |
| 502 | every upstream failed or verification failed |
| 504 | offline and not cached, or upstream timeout with nothing to serve |
