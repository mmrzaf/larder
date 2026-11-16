# Cache model

## Classes

| Class | Meaning | Examples |
|---|---|---|
| immutable | Bytes are determined by the name; never revalidated | npm tarball, PyPI wheel, `.crate`, Maven release jar, Go `.zip`, `.deb` in `pool/`, apt `by-hash/` file |
| mutable | Content may change; revalidated | npm packument, PyPI index, Maven `maven-metadata.xml`, apt `Release` and `Packages`, Go `@v/list`, crates index file |
| passthrough | Not cached | npm audit, search |
| deny | Not served | paths outside the protocol |

Each kind owns its classification table (see [ecosystems](../03-ecosystems/index.md)). Anything a kind does not recognize is `mutable` unless the kind says `deny`, because treating unknown content as mutable is the safe direction.

## Keys and variants

`key = normalized_path [ "#" variant ]`

- Normalized path: percent-encoding preserved, duplicate slashes collapsed, no query string unless the kind opts in (`generic` can set `keep_query`).
- Variant: `abbrev` and `full` for npm packuments; `html` and `json` for PyPI indexes; empty otherwise.

## Mutable freshness

| Setting | Default | Meaning |
|---|---|---|
| `fresh_ttl` | per kind (below) | Serve without contacting upstream for this long after the last successful validation. |
| `revalidate_timeout` | 6 s | Total time allowed for a revalidation attempt before falling back to stale. |
| `stale_while_revalidate` | 0 (off) | If set, objects stale by less than this are served immediately and refreshed in the background. |
| `max_stale` | unlimited | Maximum age of a stale object that may still be served. Unlimited by default because the purpose is availability. |

Default `fresh_ttl` per kind: go 60 s, maven 300 s, npm 60 s, pypi 60 s, crates 60 s, rustup 300 s, apt 300 s, generic 300 s. Per-rule overrides are allowed (for example apt `by-hash` is immutable so TTL does not apply).

## Generations

Each revalidation that returns changed bytes (different sha256) creates a new generation; the previous generation stays as history. `metadata_history` (default 2) previous generations are kept per key. History is the safety net against a bad upstream response that passed all checks, and it is the first thing garbage collection removes. A 304 only updates `validated_at`.

`larder objects <repo> <key> --history` lists generations; `larder rollback <repo> <key>` makes the previous generation current.

## Negative cache

- A 404/410 from all upstreams for a key with no stored object is remembered in memory for `negative_ttl` (default 60 s, per repo) to avoid hammering upstreams. It is never written to disk.
- If an object is stored and upstream says 404/410 later, the stored object keeps being served (`serve_gone: true`, the default) with `X-Larder-Cache: GONE`. Set `serve_gone: false` to honor upstream removal.

## Content rules

- Objects are stored as received after content decoding. If the upstream sent gzip, Larder decodes and stores the raw bytes (the sha256 refers to raw bytes). When serving, Larder re-encodes with gzip only if the client accepts it and the stored object is text.
- `Content-Type` is stored and served as received, with a per-kind fallback if absent.
- Immutable objects with `Content-Type: text/html` are rejected for kinds that serve binary artifacts (see [integrity](integrity.md)).
- Size limit: `max_object_size` (default 2 GiB). Larger objects are streamed with `X-Larder-Cache: BYPASS` and not stored.
