# `larder warm`

Pre-fetches the exact artifacts a project needs, discovered from files the project already has, by requesting them through the running Larder. Because the requests go through the real HTTP path, everything is cached, verified, rewritten, and counted exactly as for a normal client.

```
larder warm [flags] <path>...
```

`<path>` is a file or directory. Directories are scanned recursively (skipping `node_modules`, `.git`, `vendor`, `target`, `.venv`, `dist`, `build`) for known files.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--server URL` | from config | Larder public base URL to request through |
| `--concurrency N` | 4 | parallel downloads |
| `--pin-set NAME` | none | pin everything fetched into this set (created if absent) |
| `--max-bytes SIZE` | none | stop planning when the plan's known total exceeds this; also refuse to start if it would exceed free budget |
| `--dry-run` | off | print the plan (items, known sizes) and exit |
| `--only go,npm,...` | all | restrict ecosystems |
| `--platform TAG` | none | PyPI/npm optional platform filter for lockfiles that list many files |
| `--from-cache KIND[=PATH]` | none | derive the list from a local client cache (below) |
| `--url-list FILE --repo NAME` | none | fetch paths from a text file for a generic repo |
| `--fail-fast` | off | stop at the first failure |
| `--retries N` | 2 | retries per item |

## Recognized files

| File | Ecosystem | What is derived |
|---|---|---|
| `go.sum` | go | every `module version` pair (`/go.mod` lines need only `.mod`; others need `.info`, `.mod`, `.zip`) |
| `package-lock.json`, `npm-shrinkwrap.json` | npm | packument per package, tarball per `resolved` |
| `pnpm-lock.yaml` | npm | packages and tarballs |
| `yarn.lock` | npm | classic and berry |
| `bun.lock` | npm | text lockfile |
| `requirements*.txt` | pypi | lines pinned with `==`; unpinned lines are reported as skipped |
| `poetry.lock`, `uv.lock`, `Pipfile.lock`, `pdm.lock` | pypi | package and version indexes; file URLs when the lockfile carries them |
| `Cargo.lock` | crates | registry packages (`source = "registry+..."`) |
| `gradle.lockfile`, `gradle/verification-metadata.xml` | maven | components and artifacts |
| `pom.xml` | maven | not resolved; reported as skipped with a hint to use `--from-cache` or `seed` |
| `*.uris`, `apt-packages.txt` | apt | URIs from `apt-get install --print-uris -y ...`; paths under known mounts or upstream bases are fetched |

## Local client caches (`--from-cache`)

Machines that already built the project hold exactly the files needed. `larder warm --from-cache go=~/go/pkg/mod/cache/download` walks the directory and fetches the same keys through Larder. Supported: `go` (GOPROXY download cache layout), `maven` (`~/.m2/repository`), `gradle` (`~/.gradle/caches/modules-2/files-2.1`), `cargo` (`~/.cargo/registry/cache/*/*.crate` plus index paths computed from names). When no upstream is reachable, use [`larder ingest`](storage-commands.md) instead.

## Behaviour

1. Discover files and parse them. Unknown or unparseable files are reported, not fatal.
2. Build a plan: a deduplicated ordered list of `(repo, path)` requests. Metadata documents come before the artifacts they describe (so expectations exist before artifacts are verified).
3. Map each ecosystem to the repo whose kind matches. If several repos of one kind exist, `--repo-map npm=npm-main` picks one; otherwise the first by configuration order is used.
4. Execute with the requested concurrency using `GET` (body discarded after Larder has stored it). The `X-Larder-Cache` header tells whether the item was already cached.
5. With `--pin-set`, pin each fetched key through the admin API.
6. Print a summary: items planned, already cached, fetched, failed, bytes fetched, elapsed, pinned.

Exit code is 1 if any item failed after retries.

## Storage safety

`--max-bytes` and the budget check protect the disk. Before downloading, `warm` asks the admin API for free budget; when the plan's known size would push usage above the high watermark it refuses unless `--force`.

## Examples

```
larder warm ./services/api ./services/web --pin-set myapp
larder warm --dry-run ~/work
larder warm --from-cache maven=~/.m2/repository --pin-set jvm-baseline
```
