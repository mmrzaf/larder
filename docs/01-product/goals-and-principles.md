# Goals and principles

## Purpose

Keep builds, installs, and deployments working when public registries are unreachable, blocked, rate limited, or have removed a package. Larder is a pull-through cache that remembers everything useful and forgets only on purpose.

## Primary goals

1. **Survive disconnection.** Anything fetched once through Larder can be served with no upstream at all.
2. **Reach what is reachable.** Upstreams can be official registries or regional mirrors, tried in order with failover, optionally through an outbound proxy.
3. **Be correct.** Larder never serves a corrupted, truncated, or substituted artifact. It verifies against the digests the ecosystems publish.
4. **Respect a finite disk.** Storage is bounded by an explicit budget. Eviction is predictable, explainable, and controllable by hand.
5. **Stay small.** One static binary, one data directory, one YAML file. No external database, no web UI, no plugin runtime.

## Principles

| Principle | Consequence |
|---|---|
| Immutable means forever | Content addressed by version never changes; once verified and stored it is served without contacting upstream. |
| Stale beats nothing | A mutable index that cannot be revalidated is served from the last good copy and marked stale. |
| Never replace good with bad | Error pages, truncated bodies, and failed verification never overwrite or enter the cache. |
| Do not touch bytes you do not have to | Signed or hashed content is served byte for byte. Only the few documents that embed absolute URLs are rewritten, and the rewriting happens when serving, not when storing. |
| Read only | Larder never publishes, deletes, or modifies anything upstream. Only `GET` and `HEAD` reach upstream (plus one local stub, see npm). |
| Fail fast offline | When no upstream is reachable, requests fail in milliseconds with a clear status, not after a client timeout. |
| Everything is observable | Every response says whether it was a hit, miss, revalidation, or stale; metrics and logs explain what the cache is doing. |
| Boring storage | Content-addressed blobs plus an embedded SQLite index. The index can be backed up and the blobs can be exported as a plain static tree. |
| Explicit budgets | Nothing grows without a limit; every limit has a documented default and an override. |

## Quality bar

- Every behaviour in these documents has an automated test (see the [test matrix](../07-engineering/test-matrix.md)).
- Hermetic tests run with no network: fake upstreams built with `net/http/httptest`.
- A crash at any point leaves a consistent store: no partial object is ever visible.
- Memory use does not grow with object size: bodies are streamed.
