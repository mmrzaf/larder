# Security

## Deployment assumptions

Larder runs on a private network or behind a reverse proxy such as Traefik. It has no user authentication. It is read-only toward upstreams and has no endpoint that executes anything.

## Threats and mitigations

| Threat | Mitigation |
|---|---|
| Path traversal in request paths | Reject `..`, encoded dot segments, backslashes, NUL, control characters. Keys are normalized before any filesystem use; file names on disk derive only from sha256 digests, never from request paths. |
| Server-side request forgery | Upstream hosts come only from configuration. The PyPI `_files` route is restricted to the repo's allowed file hosts. Redirect targets must not be private or link-local addresses. `generic` never accepts a client-supplied host. |
| Redirect to internal addresses | Redirects from an upstream to loopback, link-local, or private addresses are refused (`block_private_redirects`, default true), unless the target host is itself a configured upstream. |
| Open proxy abuse | Only configured repos are served, only the paths each kind classifies. Unknown paths return 404. |
| Disk exhaustion by hostile requests | Budget, quotas, `max_object_size`, emergency bypass. Optional `access.allow_cidrs`. |
| Cache poisoning by a hostile or broken upstream | Content checks, expectations, `require_verified`, history generations, rollback command. |
| Cache poisoning by client request headers | Cache keys derive from path and a small set of kind-defined variants; no other request header influences the key. |
| Information leakage through the admin API | Admin API on a separate listener, default loopback only. |
| Secret leakage | Upstream header values and proxy credentials are never logged or exposed; config printing redacts them. |
| Decompression bombs | Output limits on every decompression done for parsing. |
| Header injection | Header values from upstream are not forwarded verbatim except a short allow list (`Content-Type`, `ETag`, `Last-Modified`). |
| Request smuggling or oversized requests | Standard library server limits plus `max_header_bytes` (default 64 KiB). |
| Denial of service by many concurrent misses | Singleflight, fetch concurrency cap, per-host caps. |

## Forwarded headers

`trust_forwarded` (default false) controls whether `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host` are used for the CIDR check and for generating rewritten URLs. Enable it only behind a proxy that sets them. The simplest alternative is a fixed `public_base_url`.

## Access list

`access.allow_cidrs`: list of CIDRs allowed on the public listener. Empty means everyone who can reach the port. Requests outside the list get 403 and are counted but not otherwise processed.

## Hardening the process

- Runs as an unprivileged user; the Docker image uses a non-root UID.
- Listens on loopback by default; the container image listens on all interfaces inside the container network.
- Does not execute external programs (`warm` and `seed` call tools such as `npm` only from the CLI process, never from the server).
- File modes 0640 and directories 0750.

## Supply chain

Dependencies are few, listed in [dependencies](../07-engineering/dependencies.md), pinned by `go.sum`, and built with `-trimpath`. The Docker image is built from a static binary on a minimal base image.
