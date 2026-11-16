# Larder documentation

Larder is a single-binary, self-hosted, write-through package cache. It sits between your machines and the public package registries, keeps every artifact it has ever served, and keeps serving them when the upstream is slow, blocked, or gone.

## Sections

| # | Section | Contents |
|---|---|---|
| 01 | [Product](01-product/index.md) | Goals, principles, scope, scenarios, glossary |
| 02 | [Architecture](02-architecture/index.md) | Components, request lifecycle, cache model, storage, integrity, storage management, security, observability |
| 03 | [Ecosystems](03-ecosystems/index.md) | Go, Maven/Gradle, npm, PyPI, crates.io/rustup, apt, generic |
| 04 | [CLI](04-cli/index.md) | Every command, the admin API |
| 05 | [Configuration](05-configuration/index.md) | Full reference, example file, client setup for each tool |
| 06 | [Operations](06-operations/index.md) | Deployment, storage runbook, backup, offline drill, troubleshooting |
| 07 | [Engineering](07-engineering/index.md) | Project layout, kind interface, standards, dependencies, testing, test matrix |

## One-paragraph summary

Larder serves package registry traffic from path-based mounts (`/go`, `/npm`, `/pypi`, `/crates`, `/maven/central`, `/apt/debian`, ...). Each mount has one or more upstreams (official registries or reachable mirrors, optionally through an HTTP or SOCKS5 proxy). Immutable artifacts are stored once, verified against the digests the protocol provides, and served from disk forever. Mutable metadata is revalidated against upstream with a short timeout and falls back to the last good copy. Disk usage is bounded by an explicit budget with automatic and manual eviction, pin sets that are never evicted, and a mode that stops caching instead of failing when the disk is full.
