# Storage and data commands

Common flags: `--repo NAME`, `--dry-run` (destructive commands), `--json`.

## Reporting

| Command | Behaviour |
|---|---|
| `larder stats` | Budget, used, free disk, pinned bytes, object counts per class, per-repo totals, hit ratio and request counts since process start (when the server is reachable), offline and bypass state. |
| `larder du --by repo\|class\|age\|top [-n 50]` | Usage grouped by repo, by class, by days since last access (buckets 1, 7, 30, 90, 180, 365, older), or the `n` largest objects. |
| `larder ls <repo> [glob] [--sort size\|atime\|hits] [--limit N] [--pinned] [--unverified]` | List objects: key, size, class, verified, last access, hits, pinned. |
| `larder objects <repo> <key> [--history]` | Details of one object: blob digest, sizes, stored encoding, upstream, timestamps, expectations, pins, and all generations. |

## Metadata history

| Command | Behaviour |
|---|---|
| `larder rollback <repo> <key> [--generation N]` | Make the previous (or specified) generation of a mutable object current. Takes effect for the next request. |
| `larder prune --history [--keep N]` | Remove generations beyond `metadata_history` (or `--keep`). |

## Pins

| Command | Behaviour |
|---|---|
| `larder pin --set NAME --repo R --match GLOB [--note TEXT]` | Add a rule to a pin set (creating it). |
| `larder pin --set NAME --repo R --key KEY` | Pin one exact key. |
| `larder unpin --set NAME [--repo R] [--match GLOB]` | Remove rules or a whole set; objects are not deleted. |
| `larder pins [--set NAME]` | Sets with rule counts, matched object counts, and pinned bytes. Includes declarative pins from the configuration, marked `config`. |

Globs follow `path.Match` extended with `**` for multiple segments. A glob matches the key without the variant suffix unless it contains `#`.

## Garbage collection and eviction

| Command | Behaviour |
|---|---|
| `larder gc [--target-bytes N] [--target-pct P] [--policy lru\|lfu\|score] [--dry-run]` | Run the tiered GC described in [storage management](../02-architecture/storage-management.md) immediately. With `--target-bytes` it evicts until `used <= N` regardless of watermarks. Prints per tier: objects and bytes removed (or that would be removed). |
| `larder evict <repo> <glob> [--include-pinned] [--older-than DAYS] [--larger-than SIZE] [--dry-run]` | Remove matching objects. Pinned objects are skipped unless `--include-pinned`. |

## Runtime control

| Command | Behaviour |
|---|---|
| `larder offline on\|off\|status` | Switch offline mode on a running server. |
| `larder fetch <repo> <path>...` | Request paths through the running server (as a client would) and print status, `X-Larder-Cache`, and size. `--pin-set NAME` pins the results. `--stdin` reads paths from standard input. |
| `larder reload` | Send SIGHUP equivalent through the admin API. |

## Moving data

| Command | Behaviour |
|---|---|
| `larder export --to DIR [--repo R] [--pinned] [--match GLOB] [--older-than-days N] [--base-url URL] [--format tree\|tar]` | Write objects as a path-mirrored static tree (default) or a tar archive. Raw upstream bytes are used, except documents that a kind rewrites, which are rewritten for `--base-url` so a plain web server can serve the tree. Hardlinks are used when `DIR` is on the same filesystem. A manifest `larder-export.json` lists repos, keys, digests, and the data version. |
| `larder import --from DIR\|FILE [--repo R] [--verify]` | Import a tree produced by `export` or a tar archive: blobs are verified by digest from the manifest, rows are created, and the current generation of each key is set. Existing objects are kept unless `--replace`. |
| `larder ingest <repo> <dir> [--class immutable] [--dry-run]` | Store files from a local directory whose layout matches the repo's paths (Go module download cache at `$GOPATH/pkg/mod/cache/download`, Maven `~/.m2/repository`, a mirror tree). Only kinds `go`, `maven`, and `generic` support it. Files must classify as immutable; mutable files are skipped. Integrity checks (zip validity, companion digests present in the directory) apply; objects are stored with `verified` according to the checks that could run. Works without any upstream. |

`export` and `import` do not require the server to be stopped. Export works from a consistent index snapshot; import writes rows and blobs transactionally.

## Examples

```
larder stats
larder du --by repo
larder ls npm 'typescript*' --sort size
larder pin --set baseline --repo go --match 'github.com/spf13/**'
larder gc --dry-run
larder evict maven-central 'com/google/guava/**' --older-than 180 --dry-run
larder export --pinned --to /mnt/usb/larder-export --base-url https://larder.example
larder ingest go ~/go/pkg/mod/cache/download
```
