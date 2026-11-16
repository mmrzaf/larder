# Storage management

Storage is a constrained resource. Larder never grows without bound, always explains what it holds, and lets the operator override every decision.

## Budget

The effective budget is the smallest of:

1. `storage.max_bytes` (0 means no fixed cap);
2. filesystem capacity minus `storage.min_free_bytes` (default 5 GiB), evaluated every `gc_interval`.

`used` counts `blobs.stored_size` for all blobs plus the index and snapshot files. The budget is exposed as `larder_budget_bytes` and in `larder stats`.

## Watermarks

| Setting | Default | Meaning |
|---|---|---|
| `high_watermark` | 0.90 | Usage above `high * budget` starts GC. |
| `low_watermark` | 0.75 | GC stops when usage is at or below `low * budget`. |
| `gc_interval` | 5 m | Usage check period. |
| `hard_free_bytes` | 1 GiB | Free disk below this value triggers emergency bypass. |

## Eviction tiers

GC removes the following in order and stops at the low watermark. Pinned objects are never removed in any tier.

| Tier | Candidates |
|---|---|
| 0 | Leftover `tmp/` files older than 1 hour, `quarantine/` entries older than `quarantine_keep_days` (default 30), snapshots beyond `snapshot_keep`. |
| 1 | Superseded metadata generations beyond `metadata_history`; blobs with refcount 0. |
| 2 | Immutable objects of repos that exceed their own `quota_bytes`, least valuable first, until each repo is within quota. |
| 3 | Immutable objects not pinned and not accessed within `protect_recent_days` (default 14), ordered by the active policy. |
| 4 | Immutable objects not pinned and accessed within `protect_recent_days`, same ordering. Reached only when tiers 0 to 3 do not suffice. |
| 5 | Current mutable objects not pinned, unused for `metadata_unused_days` (default 365). Reached only when everything else is insufficient. |

If pinned content alone exceeds the budget, GC logs an error, sets `larder_pinned_over_budget 1`, and stops evicting; Larder keeps serving and enters emergency bypass for new objects when the disk requires it.

## Policies

`eviction_policy` applies inside tiers 2 to 4.

| Policy | Order (evicted first) |
|---|---|
| `lru` (default) | oldest `last_access` first |
| `lfu` | lowest `hits`, ties by oldest `last_access` |
| `score` | highest `idle_days * size_mib * repo_weight / (1 + ln(1 + hits))` first, so large, long-idle, rarely used objects go first |

`repo_weight` is the repo's `evict_weight` (default 1.0); raise it for repos whose content is cheap to re-fetch, lower it for expensive ones.

## Per-repo controls

| Setting | Meaning |
|---|---|
| `quota_bytes` | Soft cap on immutable bytes for the repo, enforced in tier 2. 0 means no cap. |
| `evict_weight` | Multiplier for `score` policy. |
| `unused_days` | Automatic removal of immutable objects not accessed for N days, even under budget. 0 means off. |
| `protect` | List of key globs always treated as pinned (declarative pin). |

## Pins

A pin set is a named group. Members are exact keys (`pin_items`) and glob rules (`pin_rules`). Warm and seed create or extend a set automatically (`--pin-set`). Pinned objects are exempt from eviction in every tier and from `unused_days`.

```
larder pin   --set baseline --repo npm --match 'typescript/**'
larder unpin --set baseline
larder pins                       # sets, member counts, pinned bytes
```

Declarative pins in the configuration (`protect`) and pin sets behave identically; `larder pins` shows both.

## Emergency bypass

If free disk space is below `hard_free_bytes`, or the budget cannot be met after GC, Larder stops committing new immutable objects and mutable generations. Requests are still answered: misses stream through from upstream with `X-Larder-Cache: BYPASS`, hits are served as usual. `larder_bypass_active 1` and a log line announce the state; it clears automatically when usage falls below the high watermark.

## History pruning

Daily maintenance and GC tier 1 remove generations beyond `metadata_history`. `larder prune --history` runs it manually.

## Reporting

| Command | Output |
|---|---|
| `larder stats` | budget, used, free, pinned bytes, objects, hit ratio since start, bypass state |
| `larder du --by repo\|class\|age\|top` | usage split by repo, class, last-access age buckets, or the largest objects |
| `larder ls <repo> [glob] --sort size\|atime` | browse objects |
| `larder gc --dry-run` | the exact list of objects that would be evicted and the bytes freed |

## Manual control

| Command | Use |
|---|---|
| `larder gc [--target-bytes N] [--policy P]` | run GC now, optionally to a specific size |
| `larder evict <repo> <glob> [--include-pinned]` | remove objects by pattern |
| `larder prune --history` | remove old generations |
| `larder export --pinned --to DIR` | copy valuable content to another disk |

Every destructive command supports `--dry-run` and prints a summary with counts and bytes.

## Safety rules

- GC deletes the index row and decrements the blob refcount in one transaction, then unlinks the blob file. An open file descriptor of a concurrent reader stays valid (Linux semantics).
- A blob is removed only when refcount reaches 0.
- GC never runs two instances at once (one mutex).
- A GC run is bounded by `gc_max_duration` (default 5 m) and continues on the next tick.
