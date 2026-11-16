# Integrity

## Goals

1. Never store or serve truncated, substituted, or error-page content.
2. Verify against protocol-provided digests whenever one exists.
3. Detect bit rot on disk.

## Always-on checks (all kinds)

| Check | Rule |
|---|---|
| Status | Only 200 is stored. |
| Length | If `Content-Length` is present, received bytes must equal it. A premature EOF is a failure. |
| Content type | For kinds that serve binary artifacts (`go` zip, `maven` jars, `npm` tarballs, `pypi` files, `crates`, `rustup`, `apt` pool, `generic` when `reject_html: true`), `text/html` is rejected unless the path itself ends in `.html`. |
| Size | Larger than `max_object_size`: bypass, do not store. |
| Non-empty | Empty bodies are rejected for immutable objects. |
| Decompression limit | When Larder decompresses for parsing, output is capped (`max_parse_bytes`, default 512 MiB). |

## Expectations

An expectation is a `(repo, key, algo, digest)` row learned from a metadata document:

| Ecosystem | Source document | Expectation for |
|---|---|---|
| npm | packument `dist.integrity` (sha512), else `dist.shasum` (sha1) | tarball key |
| PyPI | HTML `#sha256=` fragment, JSON `hashes.sha256` | file key |
| crates | index line `cksum` (sha256) | `dl/<crate>/<version>/download` key |
| Maven | companion `.sha256`, `.sha1`, `.md5` fetched alongside the artifact | artifact key |
| apt | `Release` (hashes of index files), `Packages` (`SHA256` of each `.deb`), `by-hash` path | index files, `.deb` files |
| rustup | channel manifest `hash`/`xz_hash`, `.sha256` companions | dist artifact keys |
| Go | optional strict mode: `h1:` dirhash from the sumdb lookup | `.zip` and `.mod` keys |
| generic | configured `expect` entries, optional companion digest files | the named key |

Order of use when committing an immutable object:

1. Look up expectations for the key. If present, the digest must match; mismatch means the candidate is discarded and the next upstream is tried; if no upstream matches, the request fails with 502 and a `larder_verify_failures_total` increment.
2. If none exists, the object is stored with `verified = 0`.
3. When a document later declares an expectation for an already stored unverified object, it is checked at once: match sets `verified = 1`; mismatch moves the blob to quarantine, deletes the object, and logs an error with both digests.

`verified = 0` objects are still served. The `X-Larder-Verified` header reports the state. A configuration flag `require_verified` (per repo, default false) makes Larder reject unverified immutable objects, for environments that prefer failing to serving unchecked content.

## Re-published versions

If a registry changes the bytes behind an existing version, a new document will declare a digest that disagrees with an object that was already **verified** against the earlier digest. In that case:

- the stored object is left untouched and keeps being served;
- the original expectation row is kept (the first verified digest wins);
- the event is logged at error level as `digest_conflict` with repo, key, and both digests, and counted in `larder_verify_failures_total`;
- clients that check the new digest themselves (npm lockfile integrity, pip hashes) will report their own mismatch, which is the correct signal;
- the operator decides with `larder objects <repo> <key>` and, if the new bytes are wanted, `larder evict <repo> <key>` followed by a fresh request.

For objects that were stored **unverified**, a later disagreeing expectation is treated as corruption or substitution: the blob is quarantined and the object removed. Silent replacement never happens.

## Scrub

`larder verify` and the daily scrub slice recompute sha256 of stored blobs (decompressing gzip-at-rest first) and compare with the blob name.

| Option | Meaning |
|---|---|
| `--sample N%` | verify a random N percent |
| `--rate MB/s` | throttle disk reads |
| `--repair` | refetch corrupted objects when online |
| `--repo` | limit to one repo |

The background scrub (`scrub.daily_percent`, default 2) walks a rolling cursor so the whole store is covered over time. A corrupted blob is moved to `quarantine/<sha256>` and all object rows pointing at it are removed; counts are logged and exposed as `larder_scrub_corrupt_total`.

## Trust boundaries

Larder trusts the transport (TLS to upstream) and the digests that arrive from the same upstream only as far as the protocol's own trust model does. A malicious upstream could serve a consistent lie; clients still verify signatures and lockfile hashes (apt GPG, Go sumdb, npm lockfile integrity, pip hash-checking), which Larder never disturbs.
