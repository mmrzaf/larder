# Larder

Larder is a self-hosted, write-through package cache. It sits between build
machines and public package registries, stores every artifact it serves, and
keeps serving when an upstream is slow, blocked, or unavailable.

The complete specification lives in [`docs/`](docs/README.md).

## Support status

npm, PyPI, Debian/Ubuntu apt and Alpine apk are the production-validated paths: `scripts/acceptance.sh` runs real clients (npm, pip, apt, apk) against Larder and the real registries (clean client caches, traffic forced through Larder, restarts, an unreachable upstream, concurrent clients, a crash, a full disk). Go, Maven and Gradle, crates.io, rustup and generic mirrors are implemented and covered by hermetic tests; they have not been through that acceptance run. See [validation status](docs/03-ecosystems/index.md#validation-status).

Larder is a cache, not an availability guarantee. During an upstream outage it serves what it has already cached; anything never requested through it is unavailable, and apt's `Valid-Until` limits how long a cached `Release` stays usable ([apt](docs/03-ecosystems/apt.md#offline-and-outages-what-is-and-is-not-guaranteed)).

## Features

- Go modules and checksum database, Maven and Gradle repositories, npm, PyPI,
  crates.io, rustup distributions, Debian/Ubuntu apt archives, Alpine apk
  repositories, and generic HTTP mirrors.
- Streaming downloads with upstream failover, circuit breakers, redirects,
  proxies, TLS verification, and optional rate and connection limits. A
  download that fails midway is never stored and never ends as a clean
  response.
- Digest verification from protocol metadata: Go `h1`, npm SRI, PyPI hashes,
  crates `cksum`, Maven companions, apt `Release`/`Packages`, rustup manifests,
  and configured expectations. A stored blob that fails its size check is
  discarded and fetched again.
- Offline mode with stale serving, negative caching, and explicit response
  headers.
- Budgets, quotas, tiered garbage collection, pins, snapshots, scrubbing, and
  reindexing; space reservation for concurrent large downloads.
- `warm` from lockfiles and client caches, `seed` for toolchains and baseline
  packages, and capture-based pinning.
- `export` and `import` as a path-mirrored tree or tar archive, plus ingestion
  from local Go, Maven, and generic cache trees.
- Prometheus metrics, structured logs, health and readiness endpoints, and a
  loopback admin API.

## Build

```sh
make build          # bin/larder, static, CGO disabled
make test
make race
make cover
make docker         # larder:<version>
make acceptance     # real npm, pip, apt and apk clients; needs docker, node, network
make release        # dist/larder_<version>_linux_{amd64,arm64}.tar.gz and SHA256SUMS
```

The version is taken from the nearest git tag. Release archives are
reproducible: building the same commit twice yields identical checksums.
`scripts/release.sh <version>` verifies the checkout, runs every gate and the
acceptance suite, and builds the archives and the image; it does not tag or
publish. Publishing runs from a `v*` tag in `.github/workflows/release.yml`. A
Gitman server tracking this repository runs `.gitman.yml` on the same tag: it
builds `larder:<version>` on the host and updates the compose stack in its
deployment directory.

## Quick start with a binary

```sh
bin/larder config defaults > larder.yaml
# edit data_dir, listen, admin.listen, and repos
bin/larder -c larder.yaml config check
bin/larder -c larder.yaml serve
```

The public listener serves clients; the admin listener is for health, metrics,
and administrative commands. It has no authentication: bind it to loopback or
a private network.

## Quick start with Docker

```sh
docker run -d --name larder --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v larder-data:/data \
  -v "$PWD/larder.yaml:/etc/larder/larder.yaml:ro" \
  ghcr.io/mmrzaf/larder:1.0.0
```

Use `data_dir: /data` and `listen: 0.0.0.0:8080` in the configuration, and put
a TLS-terminating reverse proxy in front.
`deploy/compose.yaml` is a hardened compose service, `deploy/larder.service` a
systemd unit, and [deployment](docs/06-operations/deployment.md) has reverse
proxy examples (nginx, Caddy, Traefik), log rotation and upgrade notes.
The annotated example configuration is
[docs/05-configuration/example.yaml](docs/05-configuration/example.yaml).

## Minimal configuration

```yaml
version: 1
data_dir: /var/lib/larder
listen: 0.0.0.0:8080
public_base_url: https://larder.example
admin:
  listen: 127.0.0.1:9090

storage:
  max_bytes: 100GiB
  min_free_bytes: 10GiB

repos:
  - name: npm
    kind: npm
    mount: /npm
    upstreams:
      - url: https://registry.npmjs.org/

  - name: pypi
    kind: pypi
    mount: /pypi
    upstreams:
      - url: https://pypi.org
    files_hosts: [files.pythonhosted.org]

  - name: go
    kind: go
    mount: /go
    upstreams:
      - url: https://proxy.golang.org
```

See the [configuration reference](docs/05-configuration/reference.md) for all
keys and defaults, and [client setup](docs/05-configuration/client-setup.md)
for each ecosystem.

## Client setup

Point native tools at the corresponding mount:

```sh
export GOPROXY=https://larder.example/go,off
npm config set registry https://larder.example/npm/
pip config set global.index-url https://larder.example/pypi/simple/
```

Cargo uses a sparse registry replacement, apt uses a normal `deb` line, and
Maven or Gradle use the repository URL directly. Complete examples are in
[client setup](docs/05-configuration/client-setup.md).

## Storage management

```sh
larder -c larder.yaml stats
larder -c larder.yaml du --by repo
larder -c larder.yaml ls npm --sort size
larder -c larder.yaml pin --set baseline --repo go --match 'github.com/myorg/**'
larder -c larder.yaml gc --dry-run
larder -c larder.yaml gc
larder -c larder.yaml verify --sample 10
```

Usage is bounded by `storage.max_bytes` and filesystem free space. Eviction
runs in tiers from temporary files and superseded metadata through immutable
artifacts to unused metadata. Pins, quotas, and per-repo weights control what
stays. See [storage management](docs/02-architecture/storage-management.md)
and the [storage runbook](docs/06-operations/storage-runbook.md).

## Warming and offline use

```sh
larder -c larder.yaml warm ./services/api --pin-set api
larder -c larder.yaml seed baseline.yaml
larder -c larder.yaml offline on
```

`warm` discovers lockfiles and client caches and fetches the exact artifacts
through the normal server path. `seed` resolves curated package and toolchain
lists with native tools or direct requests. With a pin set, both use a capture
session so the server pins everything they fetch. The
[offline drill](docs/06-operations/offline-drill.md) verifies that clean
clients can build without an upstream.

## Moving data

```sh
larder -c larder.yaml export --pinned --to /mnt/archive/larder --base-url https://larder.example
larder -c larder.yaml import --from /mnt/archive/larder --verify
larder -c larder.yaml ingest go ~/go/pkg/mod/cache/download
```

Exports are a static tree or tar archive with a manifest; a plain web server
can serve the tree. Imports verify digests before creating rows. Ingest reads
existing Go, Maven, or generic cache trees without contacting an upstream.

## Operations

- Health: `GET /healthz`, readiness `GET /readyz`, metrics `GET /metrics` on
  the admin listener.
- CLI: `stats`, `du`, `ls`, `objects`, `pins`, `gc`, `evict`, `prune`,
  `rollback`, `verify`, `reindex`, `snapshot`, `offline`, `reload`, `fetch`,
  `warm`, `seed`, `export`, `import`, `ingest`.
- Backup and recovery:
  [backup-and-recovery](docs/06-operations/backup-and-recovery.md).
- Deployment and upgrades:
  [deployment](docs/06-operations/deployment.md).
- Troubleshooting:
  [troubleshooting](docs/06-operations/troubleshooting.md).

## Development

`scripts/ci.sh` runs formatting, vet, build, tests, race tests when a C
toolchain exists, coverage thresholds, module verification, hygiene checks,
and documentation consistency tests. `scripts/acceptance.sh` runs the real npm,
pip, apt and apk clients and fails on any skipped check. `scripts/e2e-real.sh` runs
other ecosystems' clients when `LARDER_E2E=1` is set and skips tools that are
not installed; a skip there is not a pass.

## License

MIT. See [LICENSE](LICENSE).
