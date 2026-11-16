# Deployment

Larder is one static binary with an embedded database. It needs a directory on a local disk and, optionally, a reverse proxy for TLS. No other service is required.

## Docker image

Multi-stage build: a Go builder produces a static binary (`CGO_ENABLED=0`, `-trimpath`, version injected with `-ldflags`), copied into a minimal image with CA certificates and a non-root user (UID 10001). The entrypoint is `larder`; the default command is `serve`, which reads `$LARDER_CONFIG` (`/etc/larder/larder.yaml`). `VOLUME /data`. Exposed ports: 8080 (public), 9090 (admin, internal use). Images are published as `ghcr.io/mmrzaf/larder:<version>` (`1.0.0`, `1.0`, `latest`); pin a full version in production.

The image has no shell, so the container health check uses the binary: `larder healthcheck` reads the admin address from the configuration and exits 0 or 1 (`--url` overrides it).

Run it:

```
docker run -d --name larder --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --memory 1g --stop-timeout 60 \
  -p 127.0.0.1:8080:8080 \
  -v larder-data:/data \
  -v "$PWD/larder.yaml:/etc/larder/larder.yaml:ro" \
  ghcr.io/mmrzaf/larder:1.0.0
```

Notes on the container:

- Set `data_dir: /data` and `listen: 0.0.0.0:8080` in the configuration. The default `admin.listen` of `127.0.0.1:9090` is reachable only inside the container, which is what the health check needs; do not publish it.
- A named volume is created writable by UID 10001. For a bind mount, make the directory writable by that UID (`chown 10001:10001 <dir>`), otherwise Larder refuses to start with a permission error naming the directory.
- The root filesystem can be read-only: Larder writes only below `data_dir`.
- Give the container a stop timeout longer than `shutdown_grace` (default 30 s) so a restart lets in-flight downloads finish instead of killing them.
- Memory use does not grow with object size (bodies are streamed); 512 MiB to 1 GiB covers typical use. File descriptors: the default is enough for hundreds of concurrent clients; raise `LimitNOFILE` (systemd) or `--ulimit nofile` for thousands.

## Compose

`deploy/compose.yaml` runs the same hardened container, publishes the public port on the loopback interface only, rotates container logs (5 files of 20 MiB) and requires an explicit version:

```
LARDER_VERSION=1.0.0 docker compose -f deploy/compose.yaml up -d
```

`LARDER_IMAGE` overrides the repository (for a private registry or a locally built image).

## Reverse proxy

Larder speaks plain HTTP. Put TLS and public hostnames in a reverse proxy on the same host or network. Whatever proxy you use must:

- **stream** responses without buffering them to disk or memory first (large downloads, and a cache miss streams while it is being fetched);
- not apply a response or read timeout shorter than your largest expected download over your slowest client link; hundreds of MB are normal for toolchains and Docker layers;
- pass `Host`, `X-Forwarded-Proto` and `X-Forwarded-Host`, with `trust_forwarded: true` in Larder, or fix `public_base_url`, so rewritten URLs (npm tarballs, PyPI file links) use the public name. Never enable `trust_forwarded` when Larder is reachable without the proxy.

nginx:

```
server {
    listen 443 ssl;
    server_name larder.example;
    client_max_body_size 10m;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }
}
```

Caddy:

```
larder.example {
    reverse_proxy 127.0.0.1:8080 {
        flush_interval -1
        transport http {
            response_header_timeout 1h
        }
    }
}
```

Traefik (labels on the Larder service; add `entrypoints`/`tls` for your setup):

```
- traefik.enable=true
- traefik.http.routers.larder.rule=Host(`larder.example`)
- traefik.http.services.larder.loadbalancer.server.port=8080
```

Traefik's default entrypoint read and write timeouts must be raised (or set to 0) for large downloads.

## Admin listener

The admin listener serves `/healthz`, `/readyz`, `/metrics` and the administrative API (garbage collection, eviction, offline switch, reload). It has **no authentication**. Keep it on the loopback interface (the default) or on a network that only operators and the monitoring system can reach, and never put it behind the public reverse proxy. Larder logs `admin_listener_not_loopback` at start when it is bound elsewhere.

## Logs

Larder writes structured JSON (or text, `log.format`) to standard output, one line per request plus operational events. Do not write them to the data directory. Let the runtime rotate them: Docker's `json-file` driver with `max-size`/`max-file` (as in the compose file), or journald under systemd. A busy cache logs a line per request; set `log.sample_hits` below 1 to log only a fraction of cache hits.

## Notes

- Do not publish port 9090. Scrape metrics from another container on a private network, or bind the admin listener to an address reachable only by your monitoring.
- `data_dir` must be on a local filesystem with working `rename` and `flock` (avoid network filesystems).
- Restarting is safe at any time: temporary files are discarded and every blob is flushed before the index refers to it.

## Outbound proxy sidecar

If the server reaches the wider internet through a local proxy (SOCKS5 or HTTP), put it on the same Docker network and set `upstream_proxy: socks5h://<service>:<port>`. Use per-upstream `proxy:` to send only some upstreams through it and keep reachable mirrors direct (`proxy: direct`).

## Host routing (optional)

Instead of path mounts, serve each repo on its own hostname: `routing: host` and a `host:` per repo; add a Traefik router per host (or a wildcard). Rewritten URLs use `public_url` per repo or the request host.

## systemd

`deploy/larder.service`; create the `larder` user and `/etc/larder/larder.yaml` with `data_dir: /var/lib/larder`. `TimeoutStopSec` should exceed `shutdown_grace`.

```
[Unit]
Description=Larder package cache
After=network-online.target
Wants=network-online.target

[Service]
User=larder
ExecStart=/usr/local/bin/larder serve -c /etc/larder/larder.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=2
TimeoutStopSec=60
LimitNOFILE=65536
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/larder
StateDirectory=larder

[Install]
WantedBy=multi-user.target
```

## First start checklist

1. `larder config check --connect -c larder.yaml`
2. Start the service and watch for `listening`.
3. `curl -sI http://127.0.0.1:8080/npm/lodash` shows `X-Larder-Cache: MISS`; repeat shows `HIT` or `REVALIDATED`.
4. `curl -s http://127.0.0.1:9090/readyz`
5. Point one client at Larder using [client setup](../05-configuration/client-setup.md).
6. Schedule metrics scraping and the alerts listed in [observability](../02-architecture/observability.md).
7. Run the [offline drill](offline-drill.md).

## Upgrades

Replace the binary or image and restart. Schema migrations run automatically and are forward only. A newer database refuses to start under an older binary, so **rollback means restoring the snapshot taken before the upgrade**: run `larder snapshot` (or copy the data directory while Larder is stopped) first, and keep it until the new version has proved itself. The blobs on disk are compatible across versions; only the index schema moves forward. See [backup and recovery](backup-and-recovery.md).
