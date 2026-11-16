# Project layout

Go module `github.com/<owner>/larder` (Go 1.23 or newer). Linux server target; `CGO_ENABLED=0`.

```
larder/
  cmd/larder/              main.go: subcommand dispatch only
  internal/
    config/                load, defaults, env expansion, validation, reload, redaction
    store/                 blob store: tmp files, atomic commit, gzip at rest, quarantine, usage
    index/                 SQLite: migrations, objects, blobs, expectations, pins, access-time buffer
    upstream/              HTTP client, dialers (direct, HTTP proxy, SOCKS5), breaker, failover
    cache/                 engine: lookup, singleflight, fetch, revalidate, serve, verify, negative cache
    kinds/                 Kind interface and registry
      gokind/ mavenkind/ npmkind/ pypikind/ cratekind/ rustupkind/ aptkind/ generickind/
    rewrite/               URL prefix replacement helpers
    integrity/             digest helpers, SRI parsing, scrub
    storage/               budget, usage, GC tiers and policies, pins, du, emergency state
    server/                public handler, router, admin API, middleware, health
    metrics/               registry and Prometheus text exposition
    warm/                  lockfile parsers, planner, executor
    seed/                  seed file parsing and native resolvers
    transfer/              export, import, ingest
    admin/                 admin API client used by the CLI
    version/               build information
  testutil/                fake upstreams, fault injection, fake clock, fixtures
  deploy/                  Dockerfile, compose.yaml, systemd unit, example config
  scripts/                 ci.sh, offline-drill.sh, e2e-real.sh
  docs/                    this documentation
  Makefile  LICENSE (MIT)  README.md  AGENTS.md
```

## Package rules

- `internal/` only; no public Go API.
- Dependencies point downward: `server` → `cache` → (`index`, `store`, `upstream`, `kinds`, `integrity`) → `config`. `kinds/*` depend on `kinds`, `rewrite`, `integrity`, and `index` through narrow interfaces defined in `kinds`. `storage` depends on `index` and `store`. No cycles.
- `cmd/larder` contains no logic beyond flag parsing and wiring.
- Interfaces are defined by the consumer, kept small, and exist only where a second implementation (a test fake) is needed.
- Anything time-dependent takes a `Clock` interface so tests control time.

## Responsibilities in one line each

| Package | Responsibility |
|---|---|
| `config` | turn a YAML file into a validated immutable `Config` value |
| `store` | durable, atomic file storage of blobs by digest |
| `index` | durable key to blob mapping, expectations, pins, access statistics |
| `upstream` | fetch from ordered upstreams with failover, proxies, circuit breakers |
| `cache` | decide hit, miss, revalidate, stale; coordinate fetches; verify; serve bytes |
| `kinds` | classify paths, rewrite documents, extract expectations for one ecosystem |
| `storage` | enforce budget: GC tiers, pins, emergency mode, reports |
| `server` | HTTP surface: routing, admin API, health, metrics endpoint |
| `warm`, `seed`, `transfer` | CLI-side tools built on the HTTP and admin surfaces or direct store access |
| `testutil` | everything tests share, never imported by non-test code |

## Build

`Makefile` targets: `build`, `test`, `race`, `vet`, `fmt`, `cover`, `docker`, `e2e` (real clients, opt in), `release` (static linux amd64 and arm64 with `-trimpath` and version ldflags), `clean`.
