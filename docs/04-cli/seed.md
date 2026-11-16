# `larder seed`

Fetches a curated list of packages and toolchains described in a seed file. Use it for things that are not in any project's lockfile: standard toolchains, base tools, packages you want available for new projects, platform matrices.

```
larder seed [flags] <seedfile.yaml>...
```

## Flags

`--server URL`, `--concurrency N`, `--pin-set NAME` (overrides the file's `pin_set`), `--dry-run` (resolve and print the plan, fetch nothing), `--resolver native|lockfile` (default `native` when the tool exists, otherwise `lockfile` where possible), `--max-bytes SIZE`, `--keep-workdir` (keep temporary directories for inspection).

## Seed file

```yaml
version: 1
pin_set: baseline            # everything fetched is pinned here

go:
  modules:                   # module@version
    - github.com/spf13/cobra@v1.8.1
  toolchains: ["1.23.2"]     # golang.org/toolchain for each platform
  platforms: [linux-amd64, linux-arm64]

npm:
  packages: ["typescript@5.6.2", "vite@^5", "eslint@^9"]

pypi:
  packages: ["pip", "setuptools", "wheel", "hatchling", "poetry-core", "requests>=2.32"]
  python: ["3.11", "3.12"]
  platforms: ["manylinux2014_x86_64", "manylinux_2_28_x86_64", "any"]
  only_binary: true

crates:
  packages: ["serde@1", "tokio@1", "clap@4"]

rustup:
  channels: [stable, "1.82.0"]
  targets: [x86_64-unknown-linux-gnu]
  components: [rustc, cargo, rust-std, clippy, rustfmt]

maven:
  repo: maven-central
  coordinates:               # group:artifact:version[:classifier[@ext]]
    - org.jetbrains.kotlin:kotlin-stdlib:2.0.20
  transitive: true           # requires gradle or mvn when resolver is native

gradle_dist:
  repo: gradle-dist
  versions: ["8.10.2"]       # bin and all

apt:
  repo: apt-debian
  suite: bookworm
  arch: [amd64]
  packages: [build-essential, git, curl]

generic:
  - repo: github
    paths:
      - /owner/tool/releases/download/v1.2.3/tool-linux-amd64.tar.gz
```

Every section is optional. Unknown keys are an error.

## Resolution

| Section | `native` resolver | `lockfile` resolver |
|---|---|---|
| go | temporary module; `GOPROXY` set to Larder; `go get` then `go mod download` | modules listed are fetched without dependencies unless `go.mod` of each is parsed recursively (the tool fetches `.mod` files and follows `require` lines; version selection is minimal version selection approximated by taking every listed requirement) |
| npm | `npm install --package-lock-only` in a temporary project against Larder, then warm the generated lockfile | not available |
| pypi | `pip download` per platform and Python version into a temporary directory against Larder (`--only-binary=:all:` when `only_binary`) | not available |
| crates | temporary crate with the dependencies, `cargo fetch` with source replacement to Larder | not available |
| rustup | not applicable; channel manifests are read from Larder and the listed components and targets are requested directly | same |
| maven | temporary Gradle project resolving the coordinates and `transitive` dependencies (or `mvn dependency:go-offline`) | fetch exactly the listed coordinates with their POM and artifact |
| gradle_dist | direct | direct |
| apt | metadata fetched through Larder, dependency closure computed from `Packages` (best effort), `.deb` files requested | same |
| generic | direct | direct |

`native` uses the real tools, so resolution matches what real clients do; it needs the tool installed on the machine running `seed`. A missing tool produces a clear message naming the section and the tool; other sections continue.

## Pinning and keys

All requests flow through Larder. With a pin set, the CLI starts a **capture** on the admin API (`POST /_larder/v1/capture/start`). The capture records the keys of every object served to requests that carry the capture token (the CLI's own direct requests) or that come from the machine running `seed` (requests made by native tools such as `npm` and `pip`, which cannot add headers). The CLI ends the capture when the run completes and the server pins the collected keys into the set. By default the machine's addresses are the loopback address and the addresses of its network interfaces; `--capture-from CIDR` overrides them when the server sees a different address (for example a reverse proxy address without `trust_forwarded`). Capture sessions expire after 1 hour if never closed.

## Plan output

`--dry-run` prints, per section, the resolved items and estimated sizes (from cache or upstream `Content-Length` via `HEAD`), the total, and whether it fits the budget.

## Exit behaviour

Per-section results are reported; exit code 1 if any section had failures.
