# 03 Ecosystems

Every ecosystem is a **kind**: a classifier that maps a request path to a class, key, and upstream path, plus optional rewriting and metadata-ingest hooks. The engine does everything else.

| File | Kind(s) | Special handling |
|---|---|---|
| [go.md](go.md) | `go` | GOPROXY protocol, sumdb proxying, optional strict `h1:` verification |
| [maven-gradle.md](maven-gradle.md) | `maven` (+ generic for Gradle distributions) | Metadata vs release rules, snapshots, companion checksum verification |
| [npm.md](npm.md) | `npm` | Packument variants, tarball URL rewriting, integrity from packuments, audit handling |
| [pypi.md](pypi.md) | `pypi` | HTML and JSON index rewriting, `_files` route, hashes from index |
| [crates-rustup.md](crates-rustup.md) | `crates`, `rustup` | Sparse index, synthesized `config.json`, download mapping, toolchain manifests |
| [apt.md](apt.md) | `apt` | Release and Packages parsing, by-hash, suite consistency, `Valid-Until` |
| [alpine.md](alpine.md) | `alpine` | `APKINDEX.tar.gz` mutable, `.apk` immutable, apk verifies signatures |
| [generic.md](generic.md) | `generic` | Rule-based caching of arbitrary URLs: GitHub releases, toolchains, binary downloads |

## Summary matrix

| Kind | Mutable | Immutable | Rewrites | Expectations from |
|---|---|---|---|---|
| go | `@v/list`, `@latest`, non-semver `.info`, sumdb `latest`/`supported` | `.info` `.mod` `.zip` for canonical versions, sumdb `lookup` and `tile` | none | strict mode: sumdb lookup |
| maven | `maven-metadata*`, snapshots, `archetype-catalog.xml` | everything else | none | companion `.sha512/.sha256/.sha1/.md5` |
| npm | packuments (two variants), `/-/*` GET | `*.tgz` | `dist.tarball` in packuments | `dist.integrity`, `dist.shasum` |
| pypi | `/simple/**`, JSON API | files, `.metadata` | file URLs in indexes | `#sha256=`, `hashes.sha256` |
| crates | sparse index files | `.crate` via `dl/` | `config.json` synthesized | index `cksum` |
| rustup | channel manifests, `rustup-init` | dated `dist/YYYY-MM-DD/*`, `rustup/archive/*` | none | manifest hashes, `.sha256` |
| apt | `dists/**` except by-hash | `pool/**`, `by-hash/**` | none | `Release`, `Packages`, by-hash path |
| alpine | `APKINDEX.tar.gz` | `*.apk` | none | none (apk verifies signatures) |
| generic | configured | configured | none | configured, companion files |

## Validation status

An ecosystem being implemented does not mean it has been exercised by its real client. The table states what evidence exists. "Acceptance" is `scripts/acceptance.sh`: real clients, clean client caches, real upstream registries, traffic forced through Larder (see [testing](../07-engineering/testing.md)).

| Kind | Hermetic tests (fake upstreams) | Real-client acceptance |
|---|---|---|
| npm | yes | `npm ci` and `npm install` with npm 11 on Node 24: cold, warm, concurrent, restart, upstream unreachable, audit, Docker build |
| apt | yes | `apt-get update`/`install` with Debian 12 and 13 and Ubuntu 22.04 and 24.04 (amd64; Debian arm64 indexes and packages): cold, warm, concurrent, upstream unreachable, signature enforcement |
| pypi | yes | `pip install --require-hashes` with Python 3.12: cold, concurrent, restart, upstream unreachable |
| alpine | yes | `apk update` and `apk add` with Alpine 3.20 and 3.22: cold, warm, concurrent, upstream unreachable, signature enforcement |
| go | yes | `scripts/e2e-real.sh` (opt-in, may skip) |
| maven, gradle | yes | `scripts/e2e-real.sh` (opt-in, may skip) |
| crates, rustup | yes | `scripts/e2e-real.sh` (opt-in, may skip) |
| generic | yes | none |

Kinds without acceptance coverage are implemented and tested against fake upstreams only. Treat them as supported but not production-validated until you have run them against your own clients.

## Common rules for all kinds

1. A kind never fetches from a host that is not configured for the repo.
2. Query strings are dropped from keys unless the kind says otherwise.
3. Unknown paths are denied (404); npm and pypi expose only the specific fallbacks documented in their pages.
4. Each kind documents its test scenarios in the [test matrix](../07-engineering/test-matrix.md).
5. Repos with the same kind but different upstreams are separate repos; they never share keys.

## Adding an ecosystem

Implement the `Kind` interface ([kind-interface](../07-engineering/kind-interface.md)), register it, add a page here, add a section to the [test matrix](../07-engineering/test-matrix.md), and add a client setup section in [client-setup](../05-configuration/client-setup.md).
