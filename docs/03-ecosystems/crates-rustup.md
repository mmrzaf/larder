# Rust: crates.io and rustup

## crates (`kind: crates`)

Implements the sparse index protocol and crate downloads, so cargo works through plain source replacement.

### Client (`~/.cargo/config.toml`)

```toml
[source.crates-io]
replace-with = "larder"

[source.larder]
registry = "sparse+https://larder.example/crates/index/"
```

### Paths (after the mount)

| Path | Class | Notes |
|---|---|---|
| `/index/config.json` | synthesized | `{"dl": "<public base>/<mount>/dl"}`; never fetched from upstream |
| `/index/1/<name>`, `/index/2/<name>`, `/index/3/<c>/<name>`, `/index/<ab>/<cd>/<name>` | mutable | index files; names lowercase; `fresh_ttl` 60 s |
| `/dl/<crate>/<version>/download` | immutable | `.crate` file |
| anything else | deny | |

Cargo appends `/{crate}/{version}/download` to the `dl` value when it contains no marker, which produces the `dl/...` path above.

### Upstreams

```yaml
- name: crates
  kind: crates
  upstreams:                        # index upstreams
    - url: https://index.crates.io
  dl_upstreams:                     # templates with {crate} and {version}
    - https://static.crates.io/crates/{crate}/{crate}-{version}.crate
```

Regional mirrors have their own index host and download template; each pair may be configured as an additional entry in each list. Index and download upstreams are independent lists, tried in order.

### Integrity

The ingest hook parses each index line (JSON object per line) and records `cksum` (sha256) as the expectation for `dl/<name>/<vers>/download`. Crate commits are verified against it. A `.crate` fetched without its index file in cache is stored unverified and checked when the index file arrives (cargo always reads the index first, so this is rare).

### Warm and seed

`Cargo.lock` gives `name`, `version`, and `checksum` for registry packages. `warm` fetches the index file for each crate, then the `.crate`. `seed` with `crates: {packages: [...]}` generates a temporary crate with those dependencies and runs `cargo fetch` against Larder.

### Notes

- The `api` field is intentionally absent: publish, yank, and search are unsupported.
- Git dependencies and other registries are not handled.

## rustup (`kind: rustup`)

Serves toolchain distribution files for `rustup` and `rustup-init`.

### Client

```
export RUSTUP_DIST_SERVER=https://larder.example/rustup
export RUSTUP_UPDATE_ROOT=https://larder.example/rustup/rustup
```

### Paths (after the mount, upstream `https://static.rust-lang.org` by default)

| Path | Class |
|---|---|
| `/dist/channel-rust-<channel>.toml` and `.toml.sha256`, `.toml.asc` (stable, beta, nightly, version numbers) | mutable, `fresh_ttl` 300 s |
| `/dist/<YYYY-MM-DD>/<file>` (components, dated manifests, `.sha256`, `.asc`) | immutable |
| `/rustup/dist/<target>/rustup-init[.exe]` and `.sha256` | mutable |
| `/rustup/archive/<version>/<target>/rustup-init[.exe]` and `.sha256` | immutable |
| anything else | deny |

The mapping is exact: with mount `/rustup`, request `/rustup/dist/...` fetches `<upstream>/dist/...`, and request `/rustup/rustup/dist/...` fetches `<upstream>/rustup/dist/...`.

### Integrity

The ingest hook parses channel and dated manifests (TOML, `github.com/pelletier/go-toml/v2`) and records the `hash` and `xz_hash` of each `pkg.<name>.target.<target>` section as expectations for the corresponding `dist/<date>/...` keys (the file path is the URL path after the dist host). `.sha256` companions are checked against the stored file when both are present.

rustup substitutes its dist server host in the manifest URLs it reads; the acceptance test for this kind runs a real `rustup` against Larder to confirm the behaviour with the installed version.

### Warm and seed

`seed` with `rustup: {channels: [stable, "1.82.0"], targets: [x86_64-unknown-linux-gnu], components: [rustc, cargo, rust-std, clippy, rustfmt]}` fetches each channel manifest, then the artifacts it lists for the requested targets and components. Without a toolchain list, nothing is fetched.

### Notes

- Toolchain artifacts are large (hundreds of MB per target); budget and `quota_bytes` matter. A `repo.quota_bytes` and pinning only the channels you need keeps this bounded.
- `rustup-init.sh` from `sh.rustup.rs` is not part of this kind; add it to a generic repo if needed.
