# Upstreams

An upstream is a base URL, possibly with a path (`https://mirror.example/npm/`). Request paths are appended to it.

## Per-upstream settings

| Key | Default | Meaning |
|---|---|---|
| `url` | required | Base URL (http or https). |
| `proxy` | inherited | `direct`, `env`, or a URL: `http://host:port`, `socks5://host:port`, `socks5h://host:port`, with optional `user:pass@`. Inherits the repo setting, then the global `upstream_proxy`. |
| `headers` | none | Extra request headers, for example `Authorization`. Values may use `${ENV_VAR}`. Never logged. |
| `connect_to` | none | `ip:port` to dial instead of resolving the host. TLS and `Host` still use the URL host. For networks that poison DNS. |
| `ca_file` | none | PEM certificates added to the system roots for this upstream. |
| `timeouts` | inherited | Per-upstream override of the timeouts below. |

`socks5h` resolves names through the proxy; `socks5` resolves locally. `env` honors `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`.

## Order and failover

Upstreams are tried in list order. An upstream with an open circuit is skipped, unless every upstream is open, in which case all are tried once in order (so a recovered network is noticed without waiting for the cool-down). Behaviour per outcome:

| Outcome | Action |
|---|---|
| 200 and valid | use it |
| 304 (revalidation) | use it |
| 404, 410 | try the next upstream; if all, the object does not exist there |
| 401, 403, 429, 5xx, timeout, connection error, TLS error | count a failure, try the next |
| invalid body (wrong length, HTML where binary expected, digest mismatch) | count a failure, try the next |
| 429 with `Retry-After` | open the circuit for the stated time, up to the maximum |

The first upstream that returns a valid object wins; its host is stored in `objects.upstream` and reported in `X-Larder-Upstream`.

## Circuit breaker

Per upstream (not per repo): `failure_threshold` consecutive failures (default 3) open the circuit for `open_for` (default 30 s). Each further failure after a half-open probe doubles the period up to `open_max` (default 5 min). One probe request is allowed after the period; success closes the circuit. A 404 is not a failure. State is in memory and exposed through metrics and `/_larder/v1/upstreams`.

## Timeouts

| Setting | Default | Applies to |
|---|---|---|
| `connect` | 5 s | TCP (or proxy) connect |
| `tls_handshake` | 5 s | TLS handshake |
| `response_header` | 15 s | until response headers arrive |
| `stall` | 30 s | maximum time without receiving body bytes |
| `revalidate_total` | 6 s | whole revalidation of a mutable object (overrides the others when smaller) |
| `fetch_total` | 0 (none) | whole download; downloads are bounded by `stall`, not by total time |

## Redirects

Up to 5, following `Location` to public hosts. The PyPI `_files` route has an explicit host allow list, and private or link-local redirect targets are refused; see [security](security.md). Authorization headers are dropped on cross-host redirects.

## Concurrency limits

`max_upstream_conns_per_host` (default 8) and `max_concurrent_fetches` (default 32) bound outgoing work; excess fetches queue in order. Queue wait counts toward client waiting, not toward upstream timeouts.

## TLS

System roots by default; `ca_file` adds extra roots. There is no option to skip verification.

## Outbound identity

User agent `larder/<version> (+https://github.com/<owner>/larder)`; the repository URL is set at build time. No client headers are forwarded except `Accept` (where a kind requires it).
