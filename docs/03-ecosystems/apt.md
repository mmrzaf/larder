# apt (`kind: apt`)

Caches Debian and Ubuntu style repositories. Signed files are served byte for byte, so apt's own GPG verification keeps working.

## Repos to define

One repo per upstream archive; each archive has its own host or path:

| Repo | Mount | Upstream |
|---|---|---|
| `apt-debian` | `/apt/debian` | `https://deb.debian.org/debian` |
| `apt-debian-security` | `/apt/debian-security` | `https://deb.debian.org/debian-security` |
| `apt-ubuntu` | `/apt/ubuntu` | `https://archive.ubuntu.com/ubuntu` |
| `apt-ubuntu-security` | `/apt/ubuntu-security` | `https://security.ubuntu.com/ubuntu` |
| third-party repos | any | their base URL |

Regional mirrors are alternate upstreams in the list.

## Client

`/etc/apt/sources.list.d/larder.sources` (deb822):

```
Types: deb
URIs: https://larder.example/apt/debian
Suites: bookworm bookworm-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
```

## Offline and outages: what is and is not guaranteed

With the upstream unreachable, a client that points at Larder can still run `apt-get update` and install packages that Larder has already served. This is verified for Debian 12 and 13 and Ubuntu 22.04 and 24.04 by the acceptance suite (cached packages install while the upstream refuses connections; a package never cached fails with an apt error).

What this relies on, and where it ends:

- **Signatures stay valid.** Larder serves `InRelease`, `Release` and `Release.gpg` byte for byte. It cannot re-sign or edit them, and apt verifies them as usual. Larder has no way to make an unsigned or tampered repository acceptable, and the acceptance suite checks that apt still rejects a repository it cannot verify.
- **`Valid-Until` is the limit of offline operation.** Some repositories put a `Valid-Until` date in `Release`: `debian-security` does, about a week ahead (checked in November 2026 for `bookworm-security`). Once it has passed, apt refuses the cached `Release` as expired, and Larder cannot change that, so a security suite stops working offline after about a week. The Debian main and `-updates` suites and the Ubuntu archives did not carry the field when checked, so they are not limited this way; check the `Release` of each suite you depend on. Setting `Acquire::Check-Valid-Until "false"` on the client disables only the freshness check, which also removes protection against being served an old, still correctly signed, index. Do that deliberately, for a defined outage, not as a permanent setting.
- **Only what was served is available.** Larder keeps the indexes and `.deb` files that clients requested. A package no client ever downloaded cannot be installed offline, and neither can a different version or architecture. Use `warm` or `seed` to pre-fetch, and pins to keep them.
- **Superseded packages disappear upstream.** Archives drop old versions from `pool/`; a cached copy keeps being served (`X-Larder-Cache: GONE`), but a cached index that lists a newer version than the cached `.deb` will fail on that version.
- **An outage is bounded by your cache, not by Larder's uptime.** The `Release` served during an outage is the last one Larder fetched. Security updates published during the outage are not visible.

Client settings for planned offline periods, in `/etc/apt/apt.conf.d/99larder`:

```
Acquire::Retries "0";
Acquire::Check-Valid-Until "false";
```

## Repository changes while clients are active

Archives publish a new generation of `Release` and the `Packages` files together, but not atomically. A client that fetches an old `InRelease` and then a new `Packages` file sees "Hash Sum mismatch". Larder handles this conservatively:

1. Packages and Release are checked against each other. A `Packages` file that does not match the cached `Release` is not stored; Larder revalidates `Release` and `InRelease` once and retries.
2. If the mismatch persists, the previously stored (consistent) object is served.
3. Paths under `by-hash/` are content-addressed and immutable. Debian and Ubuntu publish `Acquire-By-Hash: yes`, so apt fetches indexes by hash and gets exactly the generation named in the `InRelease` it holds, regardless of what changed since. The acceptance suite confirms that current apt clients take this path through Larder.

A client can still see one transient mismatch when an archive publishes between its `InRelease` and index requests and the repository does not support by-hash; running `apt-get update` again resolves it.

## Path classification (after the mount)

| Path | Class |
|---|---|
| `/pool/**` | immutable |
| `/dists/<suite>/**/by-hash/<ALGO>/<hex>` | immutable (digest is in the path) |
| `/dists/**` otherwise (`Release`, `InRelease`, `Release.gpg`, `Packages*`, `Sources*`, `Contents*`, `i18n/*`, `dep11/*`, `Index`, pdiffs) | mutable, `fresh_ttl` 300 s |
| `/project/**`, `/indices/**`, `/ls-lR*` | deny |
| anything else | deny |

## Ingest and integrity

On commit of `Release` or `InRelease`: parse the `SHA256` section (clearsigned wrappers are skipped without verifying the signature; dash-escaped lines are handled) and record expectations for `dists/<suite>/<path>`, plus the `Acquire-By-Hash` flag.

On commit of `Packages`, `Packages.gz`, `Packages.xz`, `Packages.bz2`: stream-decompress (limit `max_parse_bytes`), parse stanzas, and record `Filename` → `SHA256` expectations for the `.deb` keys. The batch is inserted in one transaction.

By-hash keys carry their own expectation, derived from the path.

Objects are stored without expectations when the metadata is not cached yet; they are checked when it arrives.

## Suite consistency

Mirrors update `Release` and `Packages*` together; a cache that stores them at different moments can serve a combination whose hashes do not match ("Hash Sum mismatch" in apt). Larder prevents this:

1. When a new `Release`/`InRelease` generation is stored, the freshness of every cached mutable object under the same `dists/<suite>/` is reset to stale so the next request revalidates it.
2. When committing a mutable object under `dists/<suite>/` that has an expectation from the current `Release` and the bytes do not match, the candidate is discarded, `Release` and `InRelease` are revalidated once, and the fetch is retried once. If it still mismatches, the stored object is served stale.

Clients that use by-hash avoid the problem entirely.

## Package retention

Old `.deb` files vanish from upstream pools when superseded. A cache only keeps what it has served. Warm and seed fetch exact versions when you want them kept; pinning protects them from eviction.

## Warm and seed

- `warm` accepts the URI list printed by `apt-get install --print-uris -y ...` from a machine that already points at Larder or an upstream: URIs under known mounts are fetched. Package-name lists with dependency resolution belong to `seed`.
- `seed` with `apt: {repo: apt-debian, suite: bookworm, arch: [amd64], packages: [...]}` fetches the suite metadata, resolves dependencies from the cached `Packages` (first alternative, virtual packages through `Provides`, already-listed packages skipped; best effort), and fetches the `.deb` files. The resolved list is printed so it can be reviewed.

## Gotchas

- Mount each archive separately: `archive.ubuntu.com` and `security.ubuntu.com` have different `pool` content.
- Debian's `deb.debian.org` redirects between CDNs; Larder follows redirects.
- `apt-get update` right after a long outage revalidates every index; set generous `revalidate_timeout` on the first run or run `larder fetch` for the indexes.
- Signed-By keyrings must be present on clients; Larder does not serve key material unless a generic repo is added for it.
