# Generic (`kind: generic`)

Caches arbitrary URLs under a mount according to explicit rules. Use it for toolchains, release binaries, and download hosts that npm and other tools reach during installs.

## Safe by default

A generic repo denies everything that no rule allows. This protects the disk and keeps the repo from turning into an open proxy for its upstream host.

## Configuration

```yaml
- name: github
  kind: generic
  upstreams:
    - url: https://github.com
  default_class: deny                 # deny | mutable
  keep_query: false                   # include the query string in keys
  reject_html: true                   # reject text/html bodies for immutable rules
  rules:                              # first match wins; regex on the path after the mount
    - match: '^/[^/]+/[^/]+/releases/download/[^/]+/[^/]+$'
      class: immutable
    - match: '^/[^/]+/[^/]+/releases/latest/download/[^/]+$'
      class: mutable
      fresh_ttl: 600s
  expect:                             # known digests
    - path: /owner/repo/releases/download/v1.0/tool.tar.gz
      sha256: "<hex>"
  companion:                          # optional digest files next to artifacts
    - suffix: ".sha256"
      algo: sha256
```

| Field | Meaning |
|---|---|
| `rules[].match` | RE2 regular expression applied to the path after the mount |
| `rules[].class` | `immutable`, `mutable`, or `deny` |
| `rules[].fresh_ttl` | override for mutable rules |
| `default_class` | class for paths no rule matches; default `deny` |
| `expect` | exact path to digest (sha256, sha512, sha1) |
| `companion` | for immutable misses, also fetch `<path><suffix>` and verify (digest is the first token of the body); missing companion means unverified |

## Redirects

Followed up to 5 times. Redirects to loopback, link-local, or private addresses are refused. Redirect targets are fetched with the repo's proxy settings and are not cached under their own names; the key is always the original request path.

## Examples

```yaml
- name: gradle-dist
  kind: generic
  upstreams: [{url: https://services.gradle.org}]
  rules:
    - {match: '^/distributions/gradle-[0-9][^/]*-(bin|all)\.zip(\.sha256)?$', class: immutable}

- name: nodejs-dist
  kind: generic
  upstreams: [{url: https://nodejs.org}]
  rules:
    - {match: '^/dist/v[0-9][^/]*/[^/]+$', class: immutable}
    - {match: '^/dist/index\.(json|tab)$', class: mutable}
    - {match: '^/dist/v[0-9][^/]*/SHASUMS256\.txt(\.sig|\.asc)?$', class: immutable}

- name: jetbrains
  kind: generic
  upstreams: [{url: https://download.jetbrains.com}]
  rules:
    - {match: '^/.+\.(zip|tar\.gz|dmg|exe|klib|jar)$', class: immutable}
```

## Behaviour notes

- Object keys are the request path after the mount (plus the query if `keep_query`).
- No rewriting and no parsing: generic caches bytes.
- Immutable rules should only match paths whose content never changes for a given name (versioned release assets). Do not mark `latest` URLs immutable.
- `reject_html` does not apply to paths whose rule or extension is `.html`.
- Partial or HTML error responses are rejected by the always-on integrity checks.

## Warm

`larder warm` accepts a URL list for generic repos: `larder warm --repo github --url-list urls.txt`, where each line is a path under the mount.
