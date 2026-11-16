# Scenarios

Each scenario is a situation the system must handle and is covered by the [test matrix](../07-engineering/test-matrix.md).

## S1. First install of a dependency

A developer runs `npm install` against Larder. The packument is fetched from the first reachable upstream, stored, rewritten so tarball URLs point back at Larder, and served. Each tarball is streamed to the client while being stored, verified against `dist.integrity`, and committed only if it matches.

## S2. The registry goes dark

The same install is repeated on a clean machine while every upstream is unreachable. Tarballs are served from disk. The packument cannot be revalidated, so the last stored copy is served with `X-Larder-Stale: 1`. The install succeeds.

## S3. A package is removed upstream

An author unpublishes a package. Upstream now answers 404 for the packument. Larder keeps serving the stored copy because `serve_gone` is enabled by default, and marks the response with `X-Larder-Upstream-Status: 404`.

## S4. A mirror is missing a file

The first upstream (a regional mirror) answers 404 for one artifact. Larder tries the next upstream, finds it, and caches it. The 404 is not cached as a failure for the artifact.

## S5. A captive portal returns HTML with status 200

An upstream or middlebox returns an HTML page for a `.whl` request. Content sanity checks and digest verification reject it. Nothing is stored. The client receives 502.

## S6. Many machines request the same large file at once

Fifty CI jobs request a 300 MB toolchain on a cold cache. Exactly one upstream request is made. One client receives bytes as they arrive; the others wait and then read from disk.

## S7. The disk fills up

Disk use crosses the high watermark. Garbage collection removes superseded metadata history, then objects of repos over quota, then the least recently used unpinned artifacts until usage falls to the low watermark. Pinned items are never touched. If the disk is nearly full anyway, Larder stops storing new objects and streams them through (`X-Larder-Cache: BYPASS`) instead of failing requests.

## S8. Preparing for an outage

An operator runs `larder warm ./myproject` for each project and `larder seed baseline.yaml` for the standard toolchains. Everything fetched is pinned into a named set so garbage collection cannot remove it.

## S9. Cache corruption

A disk error flips bits in a stored blob. The periodic scrub detects the mismatch, moves the blob to quarantine, and removes the index entry. The next request fetches it again (online) or reports it as missing (offline) rather than serving corrupted data.

## S10. Restart in the middle of a download

The process is killed while an artifact is being stored. On startup, temporary files are removed and the index is consistent. The artifact is simply absent and is fetched again on the next request.

## S11. Moving to a new disk

`larder export --pinned --to /mnt/new` writes a static tree. `larder import` on another instance, or a plain static web server, can serve it.

## S12. Seeding from machines that already have the packages

A developer machine has a populated `~/go/pkg/mod/cache/download` and `~/.m2/repository`. `larder ingest go ~/go/pkg/mod/cache/download` stores those files directly, so the data reaches Larder even when no upstream is reachable.
