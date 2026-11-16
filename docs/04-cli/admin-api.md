# Admin API

Served on the admin listener (default `127.0.0.1:9090`), never on the public listener. JSON in, JSON out. There is no authentication; bind it to loopback or a private interface and restrict it at the network level.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | liveness |
| GET | `/readyz` | readiness with reasons |
| GET | `/metrics` | Prometheus metrics |
| GET | `/_larder/v1/stats` | budget, usage, counters, state |
| GET | `/_larder/v1/du?by=repo\|class\|age\|top&n=` | usage breakdown |
| GET | `/_larder/v1/objects?repo=&match=&sort=&limit=&offset=` | list objects |
| GET | `/_larder/v1/objects/{repo}/{key...}` | object details with history |
| GET | `/_larder/v1/upstreams` | upstream health: circuit state, last error, last success |
| GET | `/_larder/v1/pins` | pin sets |
| POST | `/_larder/v1/pins` | add a rule or key |
| DELETE | `/_larder/v1/pins/{set}` | remove a set or its rules |
| POST | `/_larder/v1/gc` | `{dry_run, target_bytes, policy}` → job |
| POST | `/_larder/v1/evict` | `{repo, glob, include_pinned, older_than_days, larger_than, dry_run}` → job |
| POST | `/_larder/v1/prune` | `{history, keep, dry_run}` |
| POST | `/_larder/v1/rollback` | `{repo, key, generation}` |
| POST | `/_larder/v1/offline` | `{offline: true|false}` |
| POST | `/_larder/v1/reload` | reload configuration |
| POST | `/_larder/v1/verify` | `{repo, sample, rate, repair}` → job |
| POST | `/_larder/v1/export` | `{...}` → job |
| POST | `/_larder/v1/capture/start` | `{set, from: [cidr...]}` → `{token}`; collects keys served to requests that carry `X-Larder-Capture: <token>` or come from an address in `from` |
| POST | `/_larder/v1/capture/stop` | `{token, pin: true}` pins collected keys into the set |
| GET | `/_larder/v1/jobs/{id}` | job status, progress, result |

## Jobs

Long operations return `202` with `{"job": "<id>"}`. A job has `state` (`running`, `done`, `failed`), `progress` (counts and bytes), and `result`. Only one job of kinds `gc`, `evict`, `prune`, `verify` runs at a time; a second request returns 409 with the running job id. Jobs are kept in memory for 1 hour after completion.

## Errors

Error body: `{"error": "<code>", "message": "<text>"}`. Codes: `bad_request`, `not_found`, `conflict`, `internal`. Status mirrors the code (400, 404, 409, 500).

## Capture header

`X-Larder-Capture` on public-listener requests is honored only when the token matches an active capture, so an outside client cannot pin anything without the token. Native tools such as `npm` and `pip` cannot add the header, so a capture can also be bound to client addresses with `from`. Only requests from those addresses (the peer address, or the forwarded address when `trust_forwarded` is on) are collected, and only while the capture is active.
