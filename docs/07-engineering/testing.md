# Testing

## Levels

| Level | Scope | Network |
|---|---|---|
| Unit | functions and packages: classification tables, parsers, rewriting, eviction ordering, config validation | none |
| Integration | engine plus store plus index plus fake upstreams through the real HTTP handler | loopback only |
| End to end | `larder serve` subprocess plus CLI commands against fake upstreams | loopback only |
| Real-client acceptance | real `npm`, `pip`, `apt` and `apk` clients, in clean containers or with clean caches, against a real Larder binary and real registries; the release workflow gates npm only | internet, Docker, Node; nothing may be skipped in selected suites |
| Real client (opt in) | real `go`, `pip`, `cargo`, `gradle`, `rustup` and others against a running Larder with real or fake upstreams | optional, `LARDER_E2E=1` |

Hermetic levels run in `go test ./...` with no internet access.

## Shared test utilities (`testutil`)

- **FakeUpstream**: an `httptest.Server` with programmable routes: static bodies, headers, ETags, conditional handling, counters per path.
- **Fault injection** per route: delay before headers, stall mid-body, truncate (declared length larger than sent), close connection, return 5xx, return 200 HTML where binary is expected, flap (fail N times then succeed), return 404 then 200.
- **Fake clock** implementing the `Clock` interface for TTL, breaker, and GC tests.
- **Fixtures**: small but real-shaped documents for every ecosystem: npm packuments (full and abbreviated, scoped), PyPI HTML and JSON indexes, crates index lines, Go module zip/mod/info, Maven metadata and POMs, apt `Release`/`InRelease`/`Packages`, rustup manifests.
- **Environment builder**: creates a temp `data_dir`, a config with N repos pointing at fake upstreams, and a cache engine; returns handles to inspect the index and the store. CLI tests wrap it with a running server on loopback ports.

## Required technique per area

| Area | Technique |
|---|---|
| Path normalization | fuzz test (`go test -fuzz`) with a corpus of traversal attempts; invariants: no `..`, no escape from the key space |
| Parsers (npm packument ingest, PyPI HTML, apt Release/Packages, crates index, rustup TOML) | table tests with fixtures, fuzz tests for no-panic, size-limit tests |
| Rewriting | golden files: input document and base URL to expected output; idempotence (rewriting twice equals once) |
| Singleflight | N concurrent requests, exactly one upstream hit (counter), all clients get identical bytes |
| Atomic commit and crash safety | inject a failure at each step of the commit sequence; assert that no partial object is visible and the next start recovers |
| Breaker | fake clock; open, half-open, close transitions; doubling to the maximum |
| Budgets and GC | synthetic store with controlled sizes and access times; assert exact eviction order per tier and policy; pins never evicted; dry run changes nothing |
| Offline | upstream stopped; hits, stale, misses, headers, statuses; no upstream connection attempts in offline mode (counter stays 0) |
| Integrity | corrupt a blob on disk; scrub quarantines; serve never returns corrupted bytes |
| Concurrency | `-race` runs; goroutine leak check at the end of integration tests |

## Coverage

Report with `go test -cover ./...`; thresholds in [coding standards](coding-standards.md). Coverage numbers are a floor, not the goal: each scenario in the [test matrix](test-matrix.md) must exist as a named test (`TestMatrix_C05_...`) so the mapping is searchable.

## Acceptance suite (`scripts/acceptance.sh`)

`make acceptance` runs real clients against a real `larder serve` binary and the real npm, Debian and Ubuntu archives. **Nothing is skipped**: a missing tool (go, node, npm, docker, curl), an unreachable registry or a failed assertion ends the script with a non-zero status, and the last line is `PASS: <n> checks, no skips` only when every check ran. `ACCEPT_SUITES` selects suites (default `npm apt pypi alpine crash diskfull`), `ACCEPT_DISTROS` the apt images (default `debian:bookworm debian:trixie ubuntu:22.04 ubuntu:24.04`) and `ACCEPT_ALPINES` the Alpine images (default `alpine:3.20 alpine:3.22`); the tag-release workflow currently runs `ACCEPT_SUITES=npm`. Run the full default suite independently when validating PyPI, apt, Alpine, crash and disk-full behavior; those ecosystems are not yet release-gated. `ACCEPT_UPSTREAM_PROXY=env` sends Larder's own upstream traffic through `HTTPS_PROXY` on networks that require it.

Every client is forced through Larder: npm runs with a dead HTTP(S) proxy and apt with a dead `Acquire::http::Proxy` except for the Larder address, so a request that bypassed Larder would fail. Client caches are always clean (a fresh npm cache directory, a fresh container). Traffic is asserted from Larder's own request log and metrics.

| Suite | What it proves |
|---|---|
| `npm` | cold `npm ci` with a lockfile (scoped package included) installs the exact tree; tarball URLs are rewritten to the request host; six concurrent installs produce identical trees with one upstream download per tarball; after a Larder restart everything is served from cache; with Larder restarted against an upstream that refuses connections, cached dependencies still install, packuments are served stale, and an uncached package fails; `npm audit` reports advisories through Larder and fails (never reports a clean result) when the audit service is unreachable; `docker build` with the registry passed as a build argument fetches through Larder |
| `apt` | per distribution: signed `update` and `install` through Larder with by-hash fetching, security and updates suites; apt still rejects a repository whose key it cannot verify; a warm client is served entirely from cache; with the upstream unreachable `update` and `install` of cached packages work and an uncached package fails with an apt error; Debian also fetches arm64 indexes and packages; four concurrent clients on a cold cache see no hash mismatch |
| `pypi` | `pip install --require-hashes --only-binary=:all:` in a clean Python 3.12 container: hashes generated against the public index prove the wheels are the exact upstream bytes; four concurrent installs; a restart then 0 upstream wheel fetches; with the upstream unreachable cached packages install, indexes are served stale and an uncached package fails |
| `alpine` | per Alpine image: `apk update` and `apk add` through Larder; an index that cannot be verified (no trusted keys) is rejected; a warm client fetches nothing from upstream; with the upstream unreachable cached packages install and an uncached one fails; four concurrent clients |
| `crash` | `kill -9` during an install; after a restart installs succeed, every blob verifies, the index and blob store agree, no temporary file remains |
| `diskfull` | Larder on an 8 MiB filesystem serving more than that (Debian indexes and packages): the client succeeds, Larder stays up and reports the shortage, stops cleanly, starts again on the full filesystem and keeps serving |

What the suite cannot cover: repository publication racing a client against the real archives (covered by hermetic tests and `by-hash` behaviour only), real `Valid-Until` expiry, other architectures than the ones listed, and clients other than npm 11 and apt as shipped in the listed images.

The cold-cache concurrent probes depend on public registries. The Alpine probe repeats at most three times when Larder's log records a transport error to the CDN (handshake timeout, refused connection, or I/O timeout); any other failure stops the suite. Every attempt starts with an empty data directory, so a retry still exercises four clients sharing one cold cache.

## Real client checks (`scripts/e2e-real.sh`)

For each tool installed on the machine, the script starts Larder against real upstreams, runs a minimal fetch with the tool configured as in [client setup](../05-configuration/client-setup.md), repeats it with Larder offline, and compares outputs. Tools not installed are skipped with a message, which is why it is **not** an acceptance gate: a skip is not a pass. This catches protocol details that fakes cannot (for example how `rustup` rewrites manifest URLs and how `cargo` reads `config.json`).

## Performance checks

Benchmarks for: hit path (blob serve), classification, packument rewrite of a 10 MB document, apt `Packages` ingest of a large file. The hit path must serve from disk without database writes (access times are buffered). Memory during a 1 GiB download must stay flat (streamed).

The Docker image consistency test uses an ephemeral local module proxy backed by
`go env GOMODCACHE` when `GOPROXY` is not explicitly set. This exercises the
production Dockerfile without requiring a second public Go-module download
when the host already has the modules cached. `make docker` forwards `GOPROXY`
and `GO_IMAGE` to the image build, so reachable mirrors can be supplied when
`proxy.golang.org` or Docker Hub is blocked. The runtime stage is `scratch`
with the CA bundle copied from the builder, so no runtime image is pulled.
