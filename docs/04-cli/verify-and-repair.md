# verify, reindex, snapshot

## `larder verify`

Recomputes the sha256 of stored blobs (decompressing gzip-at-rest content) and compares with the blob name.

| Flag | Meaning |
|---|---|
| `--repo R` | only blobs referenced by objects of this repo |
| `--sample PCT` | random percentage |
| `--rate MB/s` | read throttle (default 50) |
| `--repair` | for each corrupt object, remove it and, if the server is online, refetch it through the running server |
| `--check-expectations` | also compare every stored object with its expectation row and report mismatches and unverified counts |
| `--json` | machine output |

Corrupt blobs are moved to `quarantine/` and their object rows deleted (unless `--dry-run`). Exit 1 when corruption is found.

## `larder reindex`

Consistency tool for the index and blob store.

| Mode | Action |
|---|---|
| `--check` (default) | report rows without blobs, blobs without rows, refcount mismatches, duplicate current generations, expectation rows for unknown keys |
| `--fix` | repair what can be repaired: delete rows whose blobs are missing, recompute refcounts, delete orphan blobs older than 1 hour, keep the newest generation as current when duplicates exist |
| `--rebuild-blobs-only` | use when the index is lost and no snapshot exists: creates a new index that contains only blob rows (no keys); the data is retrievable only through `import`, so this mode exists to avoid deleting blobs automatically while the operator decides. Prints a warning explaining the consequence. |

The server must be stopped for `--fix` and `--rebuild-blobs-only`; `--check` works while it runs. `--check` and `--fix` exit 4 when problems remain.

## `larder snapshot`

Writes a consistent copy of the index to `snapshots/` (`VACUUM INTO`). `--to FILE` writes elsewhere. `--restore FILE` replaces the index (server must be stopped; the current index is kept as `index.db.bak`). After a restore, `reindex --fix` reconciles the blob store with the restored index.

The server also writes a snapshot daily and keeps `snapshot_keep` of them.
