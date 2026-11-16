# Client setup

Replace `https://larder.example` with your Larder address (and mounts with the ones you configured). Offline-friendly settings are marked **offline**: they make failures immediate when Larder returns 504 for something it does not have.

## Go

```
export GOPROXY=https://larder.example/go
```

Fall through to nothing rather than direct access: `GOPROXY=https://larder.example/go,off`.
Larder serves the checksum database path, so `GOSUMDB` can stay at its default. Private modules: set `GOPRIVATE` so they bypass the proxy.

## npm, pnpm, yarn, bun

```
npm config set registry https://larder.example/npm/
```

| Tool | Setting |
|---|---|
| pnpm | `pnpm config set registry https://larder.example/npm/` |
| yarn classic | `yarn config set registry https://larder.example/npm/` |
| yarn berry | `.yarnrc.yml`: `npmRegistryServer: "https://larder.example/npm"` |
| bun | `bunfig.toml`: `[install] registry = "https://larder.example/npm/"` |

**offline:** `npm install --no-audit --no-fund --prefer-offline` and `npm config set fetch-retries 0`.

**CI and Docker:** use `npm ci` with a lockfile, set `NPM_CONFIG_REGISTRY` in the job, and pass it into image builds as a build argument, because `docker build` does not inherit the job's environment. Complete examples are in the [npm page](../03-ecosystems/npm.md#ci-and-docker-builds). Packages that download binaries from other hosts at install time are not covered by the registry mirror (see the same page).

## Python

```
pip config set global.index-url https://larder.example/pypi/simple/
```

| Tool | Setting |
|---|---|
| pip over HTTP | add `trusted-host = larder.example` |
| uv | `UV_DEFAULT_INDEX=https://larder.example/pypi/simple/` |
| poetry | `[[tool.poetry.source]] name="larder" url="https://larder.example/pypi/simple/" priority="primary"` |
| pdm | `pdm config pypi.url https://larder.example/pypi/simple/` |

**offline:** `pip --retries 0 --timeout 10`.

## Rust

`~/.cargo/config.toml`:

```toml
[source.crates-io]
replace-with = "larder"

[source.larder]
registry = "sparse+https://larder.example/crates/index/"

[net]
retry = 0           # offline
```

Toolchains:

```
export RUSTUP_DIST_SERVER=https://larder.example/rustup
export RUSTUP_UPDATE_ROOT=https://larder.example/rustup/rustup
```

## Maven

`~/.m2/settings.xml`:

```xml
<settings>
  <mirrors>
    <mirror>
      <id>larder</id>
      <mirrorOf>central</mirrorOf>
      <url>https://larder.example/maven/central</url>
    </mirror>
  </mirrors>
</settings>
```

Other repositories (Google, plugin portal) are separate mounts; declare them as repositories in the project or use `mirrorOf` with their ids.

## Gradle

Global init script `~/.gradle/init.d/larder.gradle.kts`:

```kotlin
val larder = "https://larder.example/maven"
settingsEvaluated {
    pluginManagement {
        repositories {
            maven(url = "$larder/gradle-plugins")
            maven(url = "$larder/central")
        }
    }
}
allprojects {
    repositories {
        maven(url = "$larder/central")
        maven(url = "$larder/google")
    }
}
```

Add `isAllowInsecureProtocol = true` inside `maven {}` when serving over plain HTTP.
Wrapper: `distributionUrl=https://larder.example/gradle-dist/distributions/gradle-8.10.2-bin.zip` in `gradle/wrapper/gradle-wrapper.properties`.
**offline:** `--offline` uses the local Gradle cache only; to use Larder without waiting on failures, keep Larder's offline mode on.

## apt

`/etc/apt/sources.list.d/larder.sources`:

```
Types: deb
URIs: https://larder.example/apt/debian
Suites: bookworm bookworm-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: https://larder.example/apt/debian-security
Suites: bookworm-security
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
```

**offline:** `/etc/apt/apt.conf.d/99larder`:

```
Acquire::Check-Valid-Until "false";
Acquire::Retries "0";
```

## Alpine (apk)

`/etc/apk/repositories`:

```
https://larder.example/alpine/v3.20/main
https://larder.example/alpine/v3.20/community
```

Keep the Alpine keys in `/etc/apk/keys`; apk verifies the index signature itself. See [alpine](../03-ecosystems/alpine.md).

## Binary hosts used during installs

Packages download extra binaries from other hosts. Define a [generic](../03-ecosystems/generic.md) repo for each and point the tool at it with its mirror variable. Check the variable name for the tool version you use:

| Tool | Typical variable |
|---|---|
| puppeteer | `PUPPETEER_DOWNLOAD_BASE_URL` |
| playwright | `PLAYWRIGHT_DOWNLOAD_HOST` |
| electron | `ELECTRON_MIRROR` |
| sharp | `npm_config_sharp_libvips_binary_host` |
| node-gyp headers | `npm_config_disturl` / `NODEJS_ORG_MIRROR` |

## Verifying a client

```
curl -sI https://larder.example/npm/lodash | grep -i x-larder
```

Expect `X-Larder-Cache: MISS` the first time and `HIT` or `REVALIDATED` afterwards.
