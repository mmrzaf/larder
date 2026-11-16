# Maven, Gradle, Kotlin (`kind: maven`)

Every JVM build tool uses the Maven repository layout, so one kind covers Maven, Gradle, Kotlin, sbt, and Ivy resolvers. Each remote repository is its own Larder repo, because the tools list them separately.

## Repos to define

| Repo name | Mount | Upstream |
|---|---|---|
| `maven-central` | `/maven/central` | `https://repo.maven.apache.org/maven2` |
| `maven-google` | `/maven/google` | `https://maven.google.com` |
| `gradle-plugins` | `/maven/gradle-plugins` | `https://plugins.gradle.org/m2` |
| others as needed | | JitPack, JetBrains, Sonatype snapshots, internal repos |

Mirrors (including regional ones) go in the `upstreams` list; their base path is part of the URL.

## Path classification

Layout: `<group path>/<artifact>/<version>/<artifact>-<version>[-<classifier>].<ext>` with optional digest and signature suffixes.

| Rule | Class |
|---|---|
| path ends with `/` | deny (404) |
| basename starts with `maven-metadata` (including `.sha1`, `.md5`, `.sha256`, `.sha512`, `.asc`) | mutable, TTL 300 s |
| basename is `archetype-catalog.xml` (any suffix) | mutable |
| a version directory ends with `-SNAPSHOT` and the basename contains the literal `-SNAPSHOT` (not a timestamp) | mutable |
| everything else, including timestamped snapshot files, `.pom`, `.module`, `.jar`, `.aar`, `.klib`, `.asc`, checksums | immutable |

Gradle Module Metadata (`.module`) and Kotlin/Native artifacts are plain files under this layout.

## Companion checksum verification

On an immutable miss of a file that is not itself a checksum or signature, Larder fetches the companion digest from the same upstream, trying `.sha512`, `.sha256`, `.sha1`, `.md5` in that order and stopping at the first 200. The body's first whitespace-separated token is the hex digest. Outcomes:

| Companion result | Effect |
|---|---|
| 200 and digest matches | stored verified; the companion file is stored as its own immutable object |
| 200 and digest differs | candidate discarded, next upstream tried |
| 404 on all four | stored unverified |
| network error | stored unverified, debug log, no failure |

Disable with `companion_fetch: false` (saves one to four small requests per new artifact).

Checksum files requested by clients are cached like any other immutable object; when a client requests `X.jar.sha1` for a stored `X.jar`, the digest is also checked against the stored blob and a mismatch is logged as `digest_conflict`.

## Gradle wrapper distributions

Gradle wrappers download `https://services.gradle.org/distributions/gradle-<ver>-bin.zip`. Define a [generic](generic.md) repo:

```yaml
- name: gradle-dist
  kind: generic
  upstreams: [{url: https://services.gradle.org}]
  rules:
    - {match: '^/distributions/gradle-[0-9][^/]*-(bin|all)\.zip(\.sha256)?$', class: immutable}
```

and set `distributionUrl=https://larder.example/gradle-dist/distributions/gradle-8.10-bin.zip` in `gradle/wrapper/gradle-wrapper.properties`. The upstream redirects to the actual download host; Larder follows redirects.

## Warm and seed

Sources for exact artifact lists: `gradle/verification-metadata.xml` (components and artifacts with sha256), `gradle.lockfile`, and a scan of `~/.m2/repository` or `~/.gradle/caches/modules-2/files-2.1` to derive `(group, artifact, version, file)` tuples. See [warm](../04-cli/warm.md). Transitive resolution for hand-written coordinate lists in a seed file requires `gradle` or `mvn` on the machine (`resolver: native`).

## Gotchas

- Android builds use Google's repo plus Central plus the Gradle plugin portal: define all three and list them in the project.
- Kotlin/JS and Kotlin/Native download toolchains and `node`/`yarn` binaries from other hosts; define [generic](generic.md) repos for them.
- A repository that returns 200 with an HTML "not found" page is rejected by the content type check.
- SNAPSHOT metadata changes frequently; keep `fresh_ttl` low (60 s) on repos that serve snapshots.
