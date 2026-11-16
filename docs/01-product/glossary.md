# Glossary

| Term | Meaning |
|---|---|
| Repo | A configured mount: a name, a kind, one or more upstreams, and options. Example: `npm`, `maven-central`. |
| Kind | The protocol implementation for a repo: `go`, `maven`, `npm`, `pypi`, `crates`, `rustup`, `apt`, `generic`. |
| Mount | The URL path prefix under which a repo is served, such as `/npm`. |
| Upstream | A base URL Larder fetches from. A repo has an ordered list. |
| Object | One cached response, identified by `(repo, key)`. |
| Key | The normalized path of an object within a repo, plus an optional variant suffix. |
| Variant | A distinct representation of one path, such as the abbreviated and full npm packument, or the HTML and JSON PyPI index. |
| Blob | A file in the content-addressed store, named by the sha256 of its raw bytes. Several objects may share a blob. |
| Class | `immutable`, `mutable`, `passthrough`, or `deny`. Decides how an object is cached. |
| Generation | One stored version of a mutable object. The newest is current. Older ones are history. |
| Fresh TTL | How long a mutable object is served without contacting upstream. |
| Revalidate | Ask upstream whether a mutable object changed, using ETag or Last-Modified. |
| Stale | A mutable object served although it could not be revalidated. |
| Expectation | A digest Larder expects an object to have, learned from protocol metadata (for example an npm `integrity` field). |
| Verified | An object whose bytes matched an expectation. Otherwise it is unverified. |
| Pin set | A named group of objects or patterns that eviction never removes. |
| Budget | The maximum disk space Larder may use for blobs and the index. |
| Watermarks | High and low usage fractions of the budget that start and end automatic eviction. |
| Bypass | Serving an object straight from upstream without storing it. |
| Quarantine | A directory holding blobs that failed verification during a scrub. |
| Circuit breaker | Per-upstream state that skips an upstream after repeated failures and retries it later. |
| Offline mode | A state in which Larder never contacts upstream. |
| Warm | Fetch the exact artifacts a project needs, determined from its lockfiles. |
| Seed | Fetch a curated list of packages and toolchains, determined by a seed file. |
| Ingest | Store files from a local path-mirrored directory directly, without any upstream. |
