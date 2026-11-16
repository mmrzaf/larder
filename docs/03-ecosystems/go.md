# Go modules (`kind: go`)

Implements the GOPROXY protocol plus checksum database proxying, so `go` never needs direct access to the internet.

## Client

```
export GOPROXY=https://larder.example/go
# or fail instead of falling through to direct when Larder lacks something:
export GOPROXY=https://larder.example/go,off
```

With this setting `go` asks `GOPROXY/sumdb/sum.golang.org/supported` and, when Larder answers 200, uses the proxy for checksum lookups too.

## Paths (after the mount)

Module paths and versions are case-encoded by Go (`!` before an uppercase letter). Larder never decodes them for storage; it validates them with `golang.org/x/mod/module` (`CheckPath` on the unescaped form, `semver` on versions) and rejects invalid input with 400.

| Path | Class | Notes |
|---|---|---|
| `/<mod>/@v/list` | mutable | fresh TTL 60 s |
| `/<mod>/@latest` | mutable | fresh TTL 60 s |
| `/<mod>/@v/<ver>.info` | immutable if `<ver>` is a canonical semver (tag or pseudo-version); mutable otherwise (`master`, `latest`, branch names) | |
| `/<mod>/@v/<ver>.mod` | immutable | non-semver versions: 404 |
| `/<mod>/@v/<ver>.zip` | immutable | large; subject to `max_object_size` |
| `/sumdb/<name>/supported` | mutable | TTL 3600 s |
| `/sumdb/<name>/latest` | mutable | TTL 60 s |
| `/sumdb/<name>/lookup/<mod>@<ver>` | immutable | |
| `/sumdb/<name>/tile/<H>/<L>/<N>[.p/<W>]` and data tiles | immutable | partial tiles are fixed by their width |
| anything else | deny | |

## Upstreams

```yaml
- name: go
  kind: go
  upstreams:
    - url: https://goproxy.example        # a reachable GOPROXY-compatible mirror
    - url: https://proxy.golang.org
  sumdb_upstreams:
    sum.golang.org:
      - https://sum.golang.org
      - https://goproxy.example/sumdb/sum.golang.org   # some mirrors proxy sumdb
```

If `sumdb_upstreams` is omitted for `sum.golang.org`, Larder tries each configured upstream's `/sumdb/sum.golang.org` path and then `https://sum.golang.org`. A sumdb name with no configuration is refused.

## Status code passthrough

Go clients rely on 404 and 410 meaning "not found" or "gone". When every upstream answers 404 or 410, Larder returns 410 if any answered 410, else 404, with the upstream's plain-text body. These answers are held only in the in-memory negative cache. All other errors become 502 or 504.

## Integrity

- Always: length, non-empty, `.zip` must open as a valid zip (central directory readable) and every entry path must start with `<mod>@<ver>/`; `.mod` must be valid UTF-8 text; `.info` must be valid JSON with a `Version` field.
- Strict mode (`verify: strict`, default `basic`): after a `.zip` or `.mod` commit, Larder fetches `/sumdb/sum.golang.org/lookup/<mod>@<ver>` (through the same sumdb upstreams), parses the `h1:` hashes, computes the dirhash of the stored object with `golang.org/x/mod/sumdb/dirhash` (`Hash1` over the zip contents; `.mod` as a single file named `go.mod`), and compares. Mismatch fails the fetch. Lookup failures do not block the commit; the object stays `verified = 0` and is checked when the lookup becomes available (a background retry runs for unverified Go objects every hour).

## Toolchains

Go toolchain downloads (`golang.org/toolchain@v0.0.1-go1.23.2.linux-amd64`) are ordinary modules and need no special handling.

## Warm and seed

`go.sum` lines give exact `(module, version)` pairs: a line with `/go.mod` needs only `.mod`; otherwise `.info`, `.mod`, `.zip`. `go.mod` files alone do not give a complete set; `warm` uses `go.sum` when present and otherwise runs `go mod download` with `GOPROXY` set to Larder when `go` is installed. See [warm](../04-cli/warm.md).

## Gotchas

- `GOFLAGS=-mod=mod` or private modules (`GOPRIVATE`) bypass the proxy; they are not cached.
- Retracted and deprecated versions stay in cache. The client reads `retract` directives from `.mod` files and `@latest` as before.
- `GONOSUMCHECK`, `GONOSUMDB`, and `GOFLAGS=-insecure` are client concerns.
