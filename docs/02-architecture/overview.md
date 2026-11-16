# Overview

## Components

```
 clients (go, npm, pip, cargo, gradle, apt, ...)
        |
        v
 +-------------------- public listener (:8080) --------------------+
 | router: longest-prefix mount match -> repo                       |
 |   access check (optional CIDR allow list)                        |
 |   repo.kind.Classify(request) -> Plan                            |
 |   cache engine: lookup / fetch / revalidate / serve / verify     |
 +------------------------------------------------------------------+
        |                     |                       |
        v                     v                       v
  index (SQLite)       blob store (files)       upstream client
  objects, blobs,      blobs/aa/bb/<sha256>      ordered upstreams,
  expectations, pins   tmp/, quarantine/         breaker, proxy dialers

 admin listener (127.0.0.1:9090): /healthz /readyz /metrics /_larder/v1/*
 background workers: access-time flusher, GC, history pruner, scrub, DB snapshot
```

## Process model

One process runs everything. Goroutines:

- one HTTP server per listener (public, admin);
- request handlers (one goroutine per request);
- download workers started by the engine, detached from the client request (see [concurrency](concurrency-and-failure.md));
- periodic workers: access-time flush (30 s), usage check and GC (`gc_interval`), daily maintenance (history prune, scrub slice, index snapshot).

A lock file `data_dir/larder.lock` (exclusive `flock`) ensures one server per data directory. CLI commands that modify maintenance state use the admin API when the server is running, or take the lock themselves when it is not. `import` and `ingest` can write through a read-only store layout and a writable SQLite handle while the server runs; `export` reads from a consistent index snapshot.

## Data flow for the three cache classes

| Class | Hit | Miss |
|---|---|---|
| Immutable | Serve blob from disk. No upstream contact. | Fetch from upstreams, stream to client and to a temp file, verify, commit. |
| Mutable | If fresh: serve. If stale: conditional request to upstream with a short timeout; on any failure serve stale. | Fetch, store as generation 1, serve. |
| Passthrough | Never cached. | Streamed from upstream (used only for a few API calls). |

## Configuration model

A repo is the unit of configuration: name, kind, mount, upstreams, proxy, TTL overrides, quota, kind-specific options. Global settings cover listeners, storage, timeouts, logging, and the offline switch. See [reference](../05-configuration/reference.md).

## Design decisions

| Decision | Rationale |
|---|---|
| Content-addressed blobs plus SQLite index | Deduplicates identical files, makes integrity checks trivial, lets an object keep several generations without duplicating unchanged bytes. |
| SQLite via a pure Go driver | Keeps the binary static and cross-compilable. |
| Store raw upstream bytes; rewrite when serving | Preserves digests and signatures, lets one stored copy serve any hostname, and keeps export reproducible. |
| Admin API on its own listener | The public listener, which may be exposed through a reverse proxy, never exposes administrative functions. |
| Kinds are compiled in | A small Go interface per ecosystem is simpler and safer than a runtime plugin system. |
| Rules, not code, for most ecosystems | `maven`, `go`, `apt`, `generic` are classification tables plus hooks; only `npm`, `pypi`, `crates`, `rustup` need rewriting or parsing code. |
