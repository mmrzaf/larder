# Backup and recovery

## What matters

| Item | Value | Recoverable by refetch? |
|---|---|---|
| `blobs/` | the cached content | only while upstream still has it |
| `index.db` | the map from keys to blobs, pins, expectations | no: without it blobs have no names |
| config file | repos and limits | by hand |

Pinned content is the part worth protecting against loss of the machine, because it may no longer exist upstream.

## Backup procedure (server running)

1. Copy blobs: `rsync -a --ignore-existing <data_dir>/blobs/ backup:/path/blobs/`.
2. Snapshot the index: `larder snapshot --to /path/index-YYYYMMDD.db`, then copy it to the backup.
3. Run the blob copy command again to pick up files created during the snapshot.
4. Copy the configuration file.

Blob files never change after creation, so copying while running is safe. The order guarantees the snapshot never references a blob missing from the backup.

For a smaller backup of what matters most: `larder export --pinned --to DIR`.

## Restore

1. Stop the server.
2. Restore `blobs/` and the config file.
3. `larder snapshot --restore index-YYYYMMDD.db`.
4. `larder reindex --fix` (removes rows for blobs missing from the backup, deletes orphans).
5. Start the server; run `larder verify --sample 10`.

## Disaster cases

| Case | Action |
|---|---|
| Index lost, snapshot available | restore the snapshot, `reindex --fix` |
| Index lost, no snapshot, blobs intact | use `larder reindex --rebuild-blobs-only` only to keep the blobs; the keys are gone. Recover content by `larder import` from an export, or by warming again (online), or by `larder ingest` from client caches |
| Blobs lost, index intact | `reindex --fix` removes dangling rows; refetch online or import an export |
| Disk full and server will not start | free space (delete `tmp/`, old `snapshots/`, `quarantine/`), then start; run `larder gc` |
| Corrupt blob found | handled by `verify`; re-request online or `--repair` |
| Machine lost | restore from backup or from an export tree |

## Export as a fallback server

`larder export --to /srv/larder-static --base-url https://larder.example` produces a tree that nginx or Caddy can serve directly (`Content-Type` from the manifest; Go sumdb and tiles included). This is the last-resort fallback if the Larder process cannot run.
