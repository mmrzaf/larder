# 04 CLI

One binary, `larder`. Global flags: `-c, --config <file>` (default `./larder.yaml`, or `$LARDER_CONFIG`), `--admin <url>` (default from config), `--json` (machine output where supported), `-v` (debug logging).

| File | Commands |
|---|---|
| [serve.md](serve.md) | `serve`, `config check`, `config print`, `config defaults`, `version` |
| [storage-commands.md](storage-commands.md) | `stats`, `du`, `ls`, `objects`, `rollback`, `pin`, `unpin`, `pins`, `gc`, `evict`, `prune`, `offline`, `fetch`, `export`, `import`, `ingest` |
| [warm.md](warm.md) | `warm` |
| [seed.md](seed.md) | `seed` |
| [verify-and-repair.md](verify-and-repair.md) | `verify`, `reindex`, `snapshot` |
| [admin-api.md](admin-api.md) | HTTP admin API used by the CLI |

## Execution modes

| Mode | Applies to | How |
|---|---|---|
| Remote | commands that change data while the server runs | the CLI calls the admin API; if the server is not reachable it falls through to direct mode. `import` and `ingest` write through a safe direct path while the server runs; `export` uses an index snapshot. |
| Direct | the same commands when the server is stopped | the CLI takes `data_dir/larder.lock` and operates on the data itself |
| Read only direct | `stats`, `du`, `ls`, `objects`, `pins` | open the database read only; safe while the server runs |

A command that needs a running server (`warm`, `seed`, `fetch`) says so and exits with code 3 if none is reachable.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | operation failed (some items failed; see output) |
| 2 | usage or configuration error |
| 3 | server not reachable |
| 4 | data directory locked or inconsistent |

## Output conventions

Human output is tables and short summaries. `--json` prints one JSON document. Destructive commands accept `--dry-run` and always print counts and bytes. Sizes accept and print binary units (`512MiB`, `20GiB`).
