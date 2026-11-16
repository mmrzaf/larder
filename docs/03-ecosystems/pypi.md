# PyPI (`kind: pypi`)

Serves the simple repository API (PEP 503 HTML, PEP 691 JSON), file downloads, and the metadata JSON API for pip, uv, poetry, pdm, and pipenv.

## Client

```
pip config set global.index-url https://larder.example/pypi/simple/
```

## Paths (after the mount)

| Path | Class | Notes |
|---|---|---|
| `/simple/` | mutable | project list; `fresh_ttl` 3600 s; large, stored gzip at rest |
| `/simple/<project>/` | mutable, variants `html` and `json` | `<project>` is normalized (PEP 503: lowercase, runs of `-_.` become `-`); a non-normalized request gets a 301 to the normalized path |
| `/<project>/json`, `/<project>/<version>/json` | mutable | metadata API used by poetry and others; rewritten like indexes |
| `/_files/<host>/<path>` | immutable | file download from an allowed host (see below) |
| path ending in `.whl .tar.gz .zip .tar.bz2 .egg .exe .msi .metadata .asc` | immutable | relative links from mirrors whose indexes use paths like `/packages/...` |
| everything else | deny | |

Variant selection: `Accept` containing `application/vnd.pypi.simple.v1+json` or `application/vnd.pypi.simple.latest+json` selects `json`; otherwise `html`. The same value is sent upstream.

## Rewriting

Index documents contain absolute file URLs (usually `https://files.pythonhosted.org/packages/...`). Larder rewrites every occurrence of `<scheme>://<host>/` where `<host>` is an allowed file host to `<public base>/<mount>/_files/<host>/`. Query strings and fragments (`#sha256=...`) are preserved. The same replacement applies to HTML `href`, JSON `url`, and `.metadata` references. Relative links are left alone and resolved by the client against Larder's own path, which the "path ending in" rule serves from the upstream.

Allowed file hosts: the hosts of all configured upstreams plus `files_hosts` (default `files.pythonhosted.org`). A request to `_files/<host>/...` for any other host returns 403.

## Integrity

At commit, the ingest hook extracts expectations:

- HTML: the `#sha256=<hex>` fragment of each link; `data-core-metadata` / `data-dist-info-metadata` values of the form `sha256=<hex>` apply to `<file>.metadata`;
- JSON: `files[].hashes.sha256` and `files[].core-metadata.sha256`.

The expectation key is the file's Larder key (`_files/<host>/<path>` for absolute links, the resolved path for relative ones). Files are verified on commit; `.metadata` likewise.

`yanked` files stay in the index as upstream publishes them.

## Defaults

`fresh_ttl` 60 s for project indexes. `negative_ttl` 60 s. `reject_html` is on.

## Warm and seed

Lockfile sources: `requirements.txt` (only pinned `==` lines; `--hash` options are used as expectations), `poetry.lock`, `uv.lock` (files with URLs), `Pipfile.lock`, `pdm.lock`. `warm` fetches the project index first, then each wheel or sdist chosen by lockfile filename. `seed` with `pypi: {packages: [...], platforms: [...], python: [...]}` runs `pip download --only-binary=:all:` for each platform tuple (or `uv pip compile` when requested) into a temporary directory against Larder.

## Gotchas

- pip chooses among files by platform tags; a cache warmed on one platform will not contain wheels for another. Warm for every platform you build on (`seed` supports a platform matrix).
- Source distributions need build backends that are themselves downloaded from PyPI at build time. Larder only caches what is requested; pre-download them by building once through Larder or by listing `setuptools`, `wheel`, `hatchling`, `poetry-core`, `flit-core` in a seed file.
- `trusted-host` is needed for pip when serving over plain HTTP.
