# npm (`kind: npm`)

Serves the npm registry protocol for npm, pnpm, yarn classic, yarn berry, and bun.

## Client

```
npm config set registry https://larder.example/npm/
```

## Path classification (after the mount)

| Path | Class | Notes |
|---|---|---|
| path ending in `.tgz` | immutable | any depth: `/<pkg>/-/<file>.tgz`, `/@scope/<pkg>/-/<file>.tgz`, and mirror-specific layouts |
| `/-/ping` | local | answered with `200 {}` without upstream contact |
| `/-/npm/v1/security/audits`, `/audits/quick`, `/advisories/bulk` (POST) | passthrough | see Audit |
| `/-/v1/search` | passthrough | not cached |
| other `/-/*` GET | mutable | for example dist-tags |
| `/<pkg>` or `/@scope%2f<pkg>` or `/@scope/<pkg>` | mutable (packument), two variants | |
| `/<pkg>/<version>` | mutable | version documents |
| everything else | deny | |

Scoped names arrive as `@scope%2fname` from npm and as `@scope/name` from some tools; both are accepted and normalized to the form the client used for the *upstream* request (the upstream sees the same encoding). The cache key uses the decoded `@scope/name` so both spellings share one entry.

## Variants

If `Accept` contains `application/vnd.npm.install-v1+json` the variant is `abbrev`, otherwise `full`. The same Accept value is sent upstream. `abbrev` and `full` are separate objects.

## URL rewriting

A packument's `dist.tarball` is an absolute URL on the registry that produced it. Larder rewrites it at serve time so tarball downloads come back to Larder. Prefixes replaced:

- every configured upstream base URL (normalized with a trailing slash), so regional mirrors work;
- `https://registry.npmjs.org/`, `http://registry.npmjs.org/`, `https://registry.npmjs.com/`, `https://registry.yarnpkg.com/`.

Each is replaced by `<public base>/<mount>/`, where the public base follows [configuration](../05-configuration/reference.md) (`public_base_url`, or derived from the request). The match is `"tarball"\s*:\s*"<prefix>` and the path suffix is preserved, so the mirror's own tarball layout keeps working because every `.tgz` path is immutable. The stored packument stays raw; the derived `ETag` changes when the base changes.

npm's default `replace-registry-host=npmjs` only substitutes the official host, so rewriting is required when the upstream is a mirror. Rewriting is always on; there is no option to disable it.

## Integrity

When a packument is committed, the ingest hook parses `versions.*.dist` and records an expectation for each tarball key (the tarball URL path after stripping a known prefix): `integrity` (SRI, preferring sha512, then sha256, then sha1) or `shasum`. Tarball commits are verified against it. A tarball fetched before its packument is stored unverified and is checked when the packument arrives. Packument bodies are parsed with a streaming decoder; a package with tens of thousands of versions must not need more than a modest, bounded amount of memory.

## Audit

`npm install` and `npm audit` send POST requests to audit endpoints. The request body is relayed unchanged, including `Content-Encoding: gzip`, which npm sets. Setting `audit`:

| Value | Behaviour |
|---|---|
| `passthrough` (default) | try upstream within `audit_timeout` (3 s); on failure answer 503 immediately with a short text body. `npm audit` then fails with an error; it never reports "found 0 vulnerabilities" for a check that did not happen. |
| `stub` | answer `200 {}` locally. npm then reports no vulnerabilities for every package, whatever is installed. Use it only on a network where the advisory service is unreachable and you accept that no audit takes place. |
| `deny` | answer 404 |

Clients can also skip it with `--no-audit` or `audit=false`. Audit results are never cached: an advisory published today must not be hidden by an answer cached last week.

## What the mirror does not cover

Larder mirrors the registry host only. Packages whose install scripts download from other hosts (puppeteer, playwright, sharp, electron, node-gyp headers, prebuilt native binaries) still contact those hosts directly, and an install that works while the registry is unreachable may still fail on them. Mirror such hosts with a [generic](generic.md) repo and the tool's own setting (`PUPPETEER_DOWNLOAD_BASE_URL`, `ELECTRON_MIRROR`, `npm_config_sharp_binary_host`, `npm_config_disturl`), or install with `--ignore-scripts` where that is acceptable.

## CI and Docker builds

Use a lockfile and `npm ci`. `npm ci` reads only the lockfile, so it needs no packument requests and installs entirely from cached tarballs once they have been downloaded once. The lockfile's `resolved` URLs may keep pointing at `registry.npmjs.org`: with a custom registry npm replaces that host with the configured one (`replace-registry-host`, default `npmjs`).

CI job on a runner:

```
npm config set registry https://larder.example/npm/
npm ci
```

or without touching user configuration: `NPM_CONFIG_REGISTRY=https://larder.example/npm/ npm ci`.

Inside `docker build` the environment of the outer pipeline is **not** inherited. A variable exported in the CI job is invisible to `RUN npm ci`; pass the registry explicitly as a build argument:

```dockerfile
FROM node:24-slim
ARG NPM_CONFIG_REGISTRY
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
```

```
docker build --build-arg NPM_CONFIG_REGISTRY=https://larder.example/npm/ .
```

An `ARG` is available to `RUN` steps as an environment variable and npm reads it. The same holds one level further out (a Docker-in-Docker or `docker compose build` step inside a CI container): give the argument to that command too, for example `build: { args: { NPM_CONFIG_REGISTRY: "${NPM_CONFIG_REGISTRY}" } }` in a compose file. A build argument is recorded in the image history; a registry URL is not secret, but credentials must not be passed this way (use a secret mount with an `.npmrc`).

The builder must be able to reach Larder. On a single host with Larder on the loopback interface, `docker build --network=host` does that; otherwise use Larder's address or a shared Docker network.

Larder passes the exact bytes npm asked for, and npm verifies each tarball against the lockfile's `integrity`, so a successful `npm ci` also proves the cache returned the right content.

## Defaults

`fresh_ttl` 60 s for packuments; tarballs never revalidate. `negative_ttl` 60 s.

## Warm and seed

Lockfile sources: `package-lock.json` (`packages.*.resolved` and `integrity`), `pnpm-lock.yaml`, `yarn.lock` (classic and berry), `bun.lock`. `warm` fetches the packument first (so expectations exist), then each tarball through Larder using the lockfile's relative path rewritten to the Larder mount. `seed` with `npm: {packages: [...]}` runs `npm install --package-lock-only` in a temporary directory against Larder, then warms the generated lockfile.

## Gotchas

- Deprecated or unpublished versions remain available because of `serve_gone`.
- Yarn berry's `npmRegistryServer` must include the mount and no trailing path beyond it.
- Offline installs should use `--prefer-offline` or `fetch-retries=0` to avoid long retry delays on 5xx.
- Larder has been validated with npm 11 on Node 24 only (see [validation status](index.md#validation-status)); pnpm, yarn and bun are implemented from the same protocol and covered by hermetic tests.
