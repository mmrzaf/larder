# Alpine (`kind: alpine`)

Caches Alpine Linux package repositories for `apk`. Indexes and packages are passed through byte for byte, so apk's own verification keeps working: `apk` checks the index signature against the keys installed on the client (`/etc/apk/keys`) and each package against the checksum in that index. Larder adds no verification of its own and cannot weaken apk's.

## Repo

One repo per archive; the upstream is the directory that contains the release branches (`v3.20`, `edge`, ...):

```yaml
- name: alpine
  kind: alpine
  mount: /alpine
  upstreams:
    - url: https://dl-cdn.alpinelinux.org/alpine
    - url: http://mirror.arvancloud.ir/alpine     # optional alternate mirror
```

Alternate mirrors listed together must mirror the same archive.

## Client

`/etc/apk/repositories`:

```
https://larder.example/alpine/v3.20/main
https://larder.example/alpine/v3.20/community
```

Keep the Alpine signing keys installed on the client (they are part of the base image). Do not use `--allow-untrusted` or `--no-check-certificate` workarounds.

## Path classification (after the mount)

Paths have the shape `/<branch>/<repository>/<architecture>/<file>`.

| Path | Class |
|---|---|
| `.../APKINDEX.tar.gz` | mutable, `fresh_ttl` 300 s |
| `.../<name>.apk` | immutable |
| anything else (release images, directory listings, malformed paths) | deny |

`<branch>` is `vMAJOR.MINOR`, `edge` or `latest-stable`.

## Offline

A client can run `apk update` and install packages Larder has already served while the upstream is unreachable: cached packages are hits and the index is served stale. Only previously requested packages are available, and the index served is the last one fetched, so newer package versions are not visible. Alpine indexes carry no expiry date, so there is no `Valid-Until` limit as in apt. Mirrors remove superseded packages; a cached package keeps being served, but a freshly fetched index may reference versions Larder never stored.

## Validation

Hermetic tests (K01 to K03) and the real-client acceptance suite `alpine` (K04): `apk update` and `apk add` in Alpine 3.20 and 3.22 containers, cold, warm after a restart, with the upstream unreachable, concurrent, and with an index that cannot be verified being rejected.
