# Test matrix

Every scenario has an ID and exists as a test named `TestMatrix_<ID>_<Description>`. Hermetic unless marked (R) for real-client checks. N12 and A10 are run by `scripts/acceptance.sh`, which fails on any skipped check; the other (R) rows are run by the opt-in `scripts/e2e-real.sh`, which may skip tools that are not installed.

## Core engine (C)

| ID | Scenario | Expected |
|---|---|---|
| C01 | immutable miss then hit | first `MISS`, second `HIT`; upstream counter 1 |
| C02 | immutable hit with upstream stopped | `HIT`, no upstream connection |
| C03 | range request on a hit | 206 with correct bytes and `Content-Range` |
| C04 | `If-None-Match` on a hit | 304 |
| C05 | 20 concurrent misses for one key | upstream counter 1; all bodies identical |
| C06 | client disconnects mid-download of a small object | object still committed |
| C07 | client disconnects mid-download of an object above `background_complete_max` | download aborted, nothing committed |
| C08 | upstream truncates the body | nothing stored; next upstream tried or 502 |
| C09 | upstream returns 5xx | not stored; failover to second upstream succeeds |
| C10 | first upstream 404, second 200 | 200 served and cached |
| C11 | all upstreams 404 | 404; second request answered from the negative cache without upstream contact |
| C12 | breaker opens after N failures | following requests skip the upstream within milliseconds; half-open probe closes it on success |
| C13 | mutable within fresh TTL | no upstream contact |
| C14 | mutable stale, upstream 304 | `REVALIDATED`, `validated_at` updated, no new generation |
| C15 | mutable stale, upstream 200 with new bytes | new generation current, previous kept as history |
| C16 | mutable stale, upstream down | `STALE`, `X-Larder-Stale: 1`, body of last good copy |
| C17 | stored object, upstream now 404 | served with `GONE` and `X-Larder-Upstream-Status: 404`; with `serve_gone: false` a 404 |
| C18 | offline mode, miss | configured status, zero upstream connections |
| C19 | offline mode, hit and stale mutable | `OFFLINE-HIT`, stale header |
| C20 | upstream returns 200 `text/html` for a binary key | rejected, not stored |
| C21 | object above `max_object_size` | streamed with `BYPASS`, not stored |
| C22 | free disk below hard floor | `BYPASS` for misses, hits unaffected |
| C23 | SIGHUP with a valid new config | repos and TTLs updated, listeners unchanged; invalid config keeps the old one |
| C24 | path traversal attempts (plain, encoded, double-encoded, backslash, NUL) | 400, no filesystem access outside the data directory |
| C25 | `_files` style route with a host not allowed | 403 |
| C26 | methods other than GET, HEAD (and npm audit POST) | 405 |
| C27 | gzip at rest | stored smaller; served gzip to gzip clients, identity to others; same sha256 |
| C28 | HEAD requests | headers equal to GET, no body, no download triggered for a miss beyond metadata policy |
| C29 | kill during download, restart | temp files removed, store consistent, object absent |
| C30 | lost index rows with blobs present | `reindex --check` reports; `--fix` removes orphans after the grace period |
| C31 | response headers on all paths | `X-Larder-Cache`, `X-Larder-Key`, `Cache-Control` correct per class |
| C32 | access-time flush | hits do not write to the database synchronously; values flushed periodically and at shutdown |
| C33 | SOCKS5 and HTTP proxy upstreams | traffic passes through the proxy (fake proxy counter); `direct` bypasses |
| C34 | per-upstream headers | sent upstream, never logged, redacted in `config print` |
| C35 | redirect following | up to 5; redirect to private address refused; auth header dropped across hosts |
| C36 | CIDR access list | allowed and denied clients; forwarded address honored only with `trust_forwarded` |
| C37 | metrics and health | `/healthz`, `/readyz` states, metrics names and labels present and updated |
| C38 | graceful shutdown | in-flight requests finish, flush runs, lock released |
| C39 | expectation mismatch on commit | candidate discarded; next upstream tried; `verify_failures` incremented |
| C40 | late expectation for an unverified stored object | match sets verified; mismatch quarantines and removes the object |
| C41 | digest conflict on a verified object | stored object kept, error logged, expectation unchanged |
| C42 | `require_verified: true` | unverified immutable object rejected with 502 |
| C43 | stale-while-revalidate window | stale served immediately, background refresh updates the object |
| C44 | host routing | repo chosen by `Host`; rewritten URLs use the repo's public URL |

## Storage management (S)

| ID | Scenario | Expected |
|---|---|---|
| S01 | usage accounting | `used` equals sum of stored blob sizes plus index files; dedup counted once |
| S02 | GC above high watermark | evicts to the low watermark in tier order |
| S03 | pinned objects | never evicted in any tier, including by `evict` without `--include-pinned` |
| S04 | per-repo quota | tier 2 trims the offending repo only, oldest first |
| S05 | policies | `lru`, `lfu`, `score` produce the documented order on synthetic data |
| S06 | metadata protection | current mutable objects removed only in tier 5 |
| S07 | history pruning | generations beyond `metadata_history` removed first |
| S08 | `--dry-run` | lists exact victims and bytes, changes nothing |
| S09 | `--target-bytes` | stops at the requested size ignoring watermarks |
| S10 | `evict` with glob, age, size filters | only matching, unpinned objects removed |
| S11 | `min_free_bytes` | effective budget shrinks when the disk is nearly full |
| S12 | emergency bypass enter and exit | enters below hard floor, exits below high watermark; metric and log |
| S13 | pinned over budget | GC stops, `larder_pinned_over_budget 1`, error logged |
| S14 | `unused_days` | unused unpinned artifacts removed even under budget |
| S15 | declarative `protect` and pin sets | equivalent behaviour; `larder pins` lists both |
| S16 | scrub | corrupted blob quarantined, rows removed, counter incremented; served bytes never corrupted |
| S17 | blob refcount | shared blob survives until its last referencing object is removed |
| S18 | GC concurrency | reads during GC succeed; two GC runs never overlap |
| S19 | snapshot and restore | restored index plus `reindex --fix` gives a consistent store |
| S20 | rollback | previous generation becomes current |

## Go (G)

| ID | Scenario | Expected |
|---|---|---|
| G01 | classification table | every row of the go page, including non-semver `.info` mutable |
| G02 | `@v/list` and `@latest` revalidation | mutable behaviour |
| G03 | `.zip`, `.mod`, `.info` miss then hit | immutable |
| G04 | invalid zip (wrong prefix, truncated) | rejected |
| G05 | 404 and 410 passthrough | statuses and bodies preserved; negative cache |
| G06 | sumdb `supported`, `latest`, `lookup`, tiles | classes per table; served through the configured sumdb upstreams |
| G07 | strict verification | correct `h1` accepted; wrong one rejected; lookup failure leaves unverified, later verified |
| G08 | case-encoded module path | `!`-encoding preserved in keys |
| G09 (R) | real `go mod download` online then offline | succeeds with `GOPROXY=...,off` |

## Maven and Gradle (M)

| ID | Scenario | Expected |
|---|---|---|
| M01 | classification table | metadata, snapshots, timestamped snapshots, releases, directories |
| M02 | companion verification | `.sha512` then `.sha256`, `.sha1`, `.md5` order; match, mismatch, absent |
| M03 | companion network error | stored unverified, no failure |
| M04 | client requests checksum of a stored artifact | cached; conflict logged on mismatch |
| M05 | `maven-metadata.xml` revalidation | mutable behaviour |
| M06 | HTML 404 page with status 200 | rejected |
| M07 | gradle-dist generic repo with redirect | follows redirect, caches under the original path |
| M08 (R) | real `gradle`/`mvn` resolving a small project online then offline | succeeds |

## npm (N)

| ID | Scenario | Expected |
|---|---|---|
| N01 | classification table | packuments, scoped names (`%2f` and `/`), tarballs, `/-/` paths |
| N02 | variants | `abbrev` and `full` stored and served separately with the right Accept upstream |
| N03 | tarball URL rewriting for official host | `dist.tarball` points at Larder |
| N04 | rewriting for a mirror upstream with a base path | rewritten; tarball path suffix preserved; tarball request succeeds |
| N05 | rewriting depends on public base | different `Host` gives different output; stored bytes unchanged; ETag differs |
| N06 | integrity expectations from packument | tarball verified by sha512, then sha256 or sha1 fallback |
| N07 | tarball before packument | stored unverified; verified when the packument arrives |
| N08 | audit modes | `passthrough` fails fast with 503 offline; `stub` returns `{}`; `deny` 404 |
| N09 | `/-/ping` | local 200 |
| N10 | large packument | bounded memory, streaming ingest |
| N11 | unpublished package | stored copy served (`GONE`) |
| N12 (R) | real `npm ci` cold, warm, after a restart and with the upstream unreachable; `npm audit`; Docker build (`scripts/acceptance.sh`, suite `npm`) | succeeds; audit never reports a clean result it did not obtain |
| N13 | audit request sent with `Content-Encoding: gzip` | the header is forwarded together with the unchanged body |

## PyPI (P)

| ID | Scenario | Expected |
|---|---|---|
| P01 | classification table | indexes, JSON API, `_files`, relative file paths, denied paths |
| P02 | project name normalization | 301 to the normalized path |
| P03 | HTML index rewriting | absolute file URLs rewritten; fragments and query kept; relative links untouched |
| P04 | JSON index rewriting | `files[].url` rewritten |
| P05 | `_files` fetch from an allowed host | immutable, verified by `sha256` from the index |
| P06 | disallowed host | 403 |
| P07 | `.metadata` files | cached with expectation from the index attributes |
| P08 | variant selection | JSON and HTML stored separately |
| P09 | root `/simple/` index | large document, gzip at rest, TTL 3600 s |
| P10 (R) | real `pip install --require-hashes` cold, concurrent, warm after a restart and with the upstream unreachable (`scripts/acceptance.sh`, suite `pypi`) | succeeds; the wheels match their hashes; an uncached package fails |

## Rust (R)

| ID | Scenario | Expected |
|---|---|---|
| R01 | crates classification table | `config.json`, index path shapes, `dl/` |
| R02 | synthesized `config.json` | `dl` equals public base plus mount |
| R03 | index file miss, hit, revalidate | mutable behaviour |
| R04 | `.crate` download via templates | immutable; tried across `dl_upstreams` |
| R05 | `cksum` verification | match accepted, mismatch rejected |
| R06 | rustup classification table | channels mutable, dated artifacts immutable, `rustup-init` paths |
| R07 | manifest hash expectations | artifacts verified against manifest hashes |
| R08 (R) | real `cargo fetch` online then offline | succeeds |
| R09 (R) | real `rustup` install of a tiny profile online then offline | succeeds; confirms manifest URL substitution |

## apt (A)

| ID | Scenario | Expected |
|---|---|---|
| A01 | classification table | pool, dists, by-hash, denied paths |
| A02 | `Release` and `InRelease` ingest | expectations for index files |
| A03 | `Packages.gz`, `.xz`, `.bz2` ingest | expectations for `.deb` files; batch insert; size limit |
| A04 | `.deb` verification | match accepted, mismatch rejected |
| A05 | by-hash path | verified by its own digest |
| A06 | suite consistency on new `Release` | dependent index objects become stale |
| A07 | mismatched `Packages` during a mirror sync | discarded, `Release` revalidated, retried once, then stale |
| A08 | signed files untouched | byte-identical to upstream |
| A09 | offline `apt-get update` flow (fake archive) | stale indexes served with the right headers |
| A10 (R) | real `apt-get update` and `install` in Debian and Ubuntu containers: cold, warm, concurrent, upstream unreachable, signature enforcement (`scripts/acceptance.sh`, suite `apt`) | succeeds with signatures verified; an unverifiable repository is rejected |

## Generic (X)

| ID | Scenario | Expected |
|---|---|---|
| X01 | default deny | unmatched paths 404 without upstream contact |
| X02 | rules first match wins | classes as configured |
| X03 | `expect` digests | match accepted, mismatch rejected |
| X04 | companion `.sha256` | verified when present, unverified when absent |
| X05 | redirect following | cached under the original path |
| X06 | `keep_query` | query included in the key only when enabled |

## CLI and tools (L)

| ID | Scenario | Expected |
|---|---|---|
| L01 | `config check` | all errors reported at once, exit 2 |
| L02 | `config print` | secrets redacted |
| L03 | `stats`, `du`, `ls`, `objects` | correct figures, `--json` valid |
| L04 | `pin`, `unpin`, `pins` | matches the storage page |
| L05 | `gc`, `evict` over the admin API and in direct mode | same results both ways |
| L06 | `warm` with go.sum, package-lock.json, requirements.txt, Cargo.lock, verification-metadata.xml fixtures | exact expected request lists; metadata before artifacts |
| L07 | `warm --from-cache` for go and maven fixture directories | expected requests |
| L08 | `warm --dry-run` and `--max-bytes` | plan printed; refuses over budget |
| L09 | `warm --pin-set` | fetched keys pinned |
| L10 | `seed` with fake native tools on `PATH` | resolver invoked, results warmed, pinned through capture |
| L11 | `seed` missing tool | clear message, other sections continue |
| L12 | `ingest` for go and maven fixture trees | stored without upstream; mutable files skipped |
| L13 | `export` tree then serve with a static server | clients can fetch; rewritten documents use `--base-url` |
| L14 | `import` of an export | objects restored and verified |
| L15 | `verify`, `reindex`, `snapshot` | as specified, including `--repair` |
| L16 | exit codes | as documented |
| L17 | `healthcheck` | exit 0 when ready, 1 otherwise |

## Alpine (K)

| ID | Scenario | Expected |
|---|---|---|
| K01 | classification table | index mutable, `.apk` immutable, releases and malformed paths denied |
| K02 | package then upstream failure | package `HIT`, index served stale, unknown paths 404 |
| K03 | upstream answers with an HTML page | not stored, 502 |
| K04 (R) | real `apk update` and `apk add` in an Alpine container: cold, warm, upstream unreachable, signature enforcement (`scripts/acceptance.sh`, suite `alpine`) | succeeds with signatures verified |

## Reliability (F)

| ID | Scenario | Expected |
|---|---|---|
| F01 | upstream dies mid-transfer while the body is being streamed to a client | the connection is aborted, no object and no temporary file remain, and the next request after recovery stores the complete body |
| F02 | upstream dies mid-transfer on a passthrough response | the connection is aborted |
| F03 | upstream dies mid-transfer for a verified object | 502 before any byte is sent, nothing stored |
| F04 | the temporary directory is not writable | 500 for each request, no upstream circuit opens, requests succeed again once the directory is fixed |
| F05 | a download does not fit in the free space | streamed with `BYPASS`, not stored |
| F06 | a stored blob is shorter than its record | the entry is discarded and the content fetched again; the damaged file is quarantined |
| F07 | a stored blob file is missing | the entry is discarded and the content fetched again |
| F08 | a damaged blob with the upstream unavailable | an error, never a success |
| F09 | downloads of many keys with identical content race a sweep of unreferenced blobs | no wrong body is served and no object references a missing blob |
| F10 | a blob sweep is requested while a blob is being published | the sweep waits for the publication to finish |
| F11 | simultaneous large downloads | reservations count bytes in progress and are returned exactly once |
| F12 | upstream presents a certificate that is not trusted | the request fails and no body is delivered |
| F13 | upstream refuses connections | each request fails and the circuit opens after repeated failures |
| F14 | storing a response fails locally | no other upstream is tried and no upstream is charged |
| F15 | a metadata document does not fit in the free space | streamed with `BYPASS`, not stored |
| F16 | the request fetching a key reports twice, as a deferred release after a panic would | the first report stands, waiters are released and the key is free again |

## Operations (O)

| ID | Scenario | Expected |
|---|---|---|
| O01 | Docker image builds and starts with the example config | `/readyz` 200 |
| O02 | compose example validates | `docker compose config` succeeds |
| O03 | offline drill script on fake upstreams | passes |
| O04 | documentation consistency | every config key documented; every CLI command in the CLI docs; every metric in the observability page is exposed |
| O05 | repository hygiene | no planning or progress remarks in any file |
