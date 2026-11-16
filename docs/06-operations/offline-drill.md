# Offline drill

Purpose: prove that clean machines can build with no upstream, before you need it. Run after setup, after upgrades, and monthly.

## Preparation

1. Warm every project: `larder warm <project dirs> --pin-set <name>`.
2. Seed baseline toolchains: `larder seed baseline.yaml`.
3. `larder pins` shows the sets and bytes.

## Drill

1. Switch Larder offline: `larder offline on`. Confirm `larder stats` shows offline, and `curl -sI https://larder.example/npm/lodash` returns `X-Larder-Cache: OFFLINE-HIT` for something cached.
2. For extra assurance, block the server's outbound access to upstreams at the network level (firewall or stopping the outbound proxy) and keep Larder online but with all circuits open.
3. On **clean** containers (no client caches), run each project's real build against Larder:

```
docker run --rm -v "$PWD:/w" -w /w node:22      sh -c 'npm config set registry https://larder.example/npm/ && npm ci --no-audit --no-fund'
docker run --rm -v "$PWD:/w" -w /w python:3.12  sh -c 'pip install --retries 0 -i https://larder.example/pypi/simple/ -r requirements.txt'
docker run --rm -v "$PWD:/w" -w /w golang:1.23  sh -c 'GOPROXY=https://larder.example/go,off go build ./...'
docker run --rm -v "$PWD:/w" -w /w rust:1      sh -c '<cargo config for larder> cargo build --release'
docker run --rm -v "$PWD:/w" -w /w gradle:8    sh -c 'gradle --no-daemon build'
docker run --rm debian:bookworm sh -c '<sources and Acquire settings for larder> apt-get update && apt-get install -y build-essential'
```

4. Record for each: success or failure, time, and any `OFFLINE-MISS` lines from the Larder log (`jq 'select(.cache=="OFFLINE-MISS")'`).
5. Every miss is something that was not warmed or seeded. Go online (`larder offline off`), warm again, and repeat.
6. Switch back: `larder offline off`.

## Pass criteria

- All builds succeed.
- Zero `OFFLINE-MISS` in the log during the builds.
- Build times are within a small factor of the online times (no timeouts being waited out).
- `larder stats` shows hits only.

## Notes

- Image base layers (the `node:22` image itself) are not Larder's concern; pull them beforehand.
- apt's `Acquire::Check-Valid-Until "false"` is required for stale `Release` files.
- Use `larder objects <repo> <key>` to see why a particular object is missing or unverified.
