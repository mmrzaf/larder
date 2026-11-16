# Storage runbook

## Sizing

Measure rather than guess: run the real workload for a week and read `larder du --by repo`. As rough orders of magnitude (verify on your data): a Rust toolchain with a couple of components per target is several hundred MB; a Gradle distribution about 100 to 250 MB; Python wheels for data and ML libraries can be GB each; npm trees for a mid-size project are hundreds of MB; a Go module cache for a mid-size service is hundreds of MB; Debian `main` indexes are tens of MB per suite and architecture.

Set `storage.max_bytes` to a figure the disk can hold with room for the index (a few percent) and snapshots, and set `min_free_bytes` so other services on the machine keep working. Use `quota_bytes` on large, cheap-to-refetch repos (rustup, pypi) so they cannot starve the rest.

## Routine

| Frequency | Check |
|---|---|
| daily | `larder stats`; alert on usage above 95% of budget, bypass active, pinned over budget |
| weekly | `larder du --by repo`, `larder du --by age`, `larder gc --dry-run` to see what the next GC would remove |
| monthly | `larder verify --sample 10`, review `larder pins`, offline drill |

## Usage is high

1. `larder stats` and `larder du --by repo`. Is one repo dominating?
2. `larder du --by top -n 30` for large objects.
3. `larder gc --dry-run` to see the eviction plan. If it removes things you care about, pin them first (`larder pin ...`) and run again.
4. Run `larder gc`.
5. If a repo should never be that large, set its `quota_bytes` and reload.
6. If everything is valuable, raise the budget, add disk, or export cold data (below).

## Pinned content exceeds the budget

`larder_pinned_over_budget 1`. GC stops evicting. Options: raise the budget, unpin sets that are no longer needed (`larder unpin --set old-project`), or shrink pinned content (`larder pins` shows bytes per set).

## Emergency bypass is active

Free disk is below `hard_free_bytes` or the budget cannot be met. Requests still work but nothing new is cached: misses are streamed to the client without being stored (`X-Larder-Cache: BYPASS`), hits are unaffected. Run `larder gc`, then fix the cause (budget, disk). Bypass ends by itself below the high watermark.

## Disk pressure, in order of escalation

1. `max_bytes` is a target. Garbage collection runs on a schedule and when usage crosses `high_watermark`; between runs a burst of downloads can exceed it. It never removes pinned content or anything a download in progress is writing.
2. `hard_free_bytes` is a hard stop. A download whose size is known is admitted only if the free space, minus the bytes already promised to downloads in progress, stays above it, so several large transfers cannot fill the disk between two checks. Otherwise it is streamed without being stored. `/readyz` reports not ready below this floor.
3. A write that still fails (full disk, quota, permissions) discards the candidate. A client already being served receives the complete body anyway; a client waiting for a verified object gets 500 (507 for no space). Local failures do not count against the upstream's circuit breaker. Look for `fetch_local_failure` in the log.
4. Out of inodes behaves like a full disk: writes fail with the same errors. Use a filesystem with enough inodes for the object count you expect (apt and npm caches hold tens of thousands of small files).
5. Larder starts on a nearly full filesystem. Leftover temporary files are removed at start, which usually frees space; the stale-entry sweep removes index rows whose blob files are gone.

After freeing space, bypass ends by itself; no restart is needed.

## Removing a project's data

```
larder unpin --set old-project
larder evict npm 'some-package/**' --dry-run
```

## Changing the budget

Edit `storage.max_bytes`, `larder config check`, then `larder reload` (or SIGHUP). If the new budget is lower, the next usage check starts GC.

## Moving the data directory

1. Stop the server.
2. `rsync -a /old/data/ /new/data/` (same layout; preserve hardlinks only if you use them).
3. Update `data_dir` or the volume mount; start the server.
4. `larder reindex --check` and `larder verify --sample 5`.

## Archiving cold data to another disk

```
larder export --match '**' --older-than-days 180 --to /mnt/archive/larder
larder evict <repo> <glob> --older-than 180
```

The archive is a static tree plus manifest; `larder import` restores it.

## Reading `larder du`

`by repo` shows where bytes are; `by class` splits immutable artifacts from metadata (metadata is small, but it is what makes offline use possible, so it is protected last); `by age` shows how much has not been used for 30, 90, 180, and more days and therefore how much `unused_days` would free.
