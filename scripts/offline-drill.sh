#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
LARDER_BIN=${LARDER_BIN:-$PWD/bin/larder}

free_port() {
    python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

wait_ready() {
    url=$1
    attempts=0
    while [ "$attempts" -lt 100 ]; do
        if "$LARDER_BIN" healthcheck --url "$url" >/dev/null 2>&1; then
            return 0
        fi
        attempts=$((attempts + 1))
        sleep 0.1
    done
    return 1
}

self_test() {
    work=$(mktemp -d)
    server_pid=
    upstream_pid=
    cleanup() {
        [ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
        [ -z "$upstream_pid" ] || kill "$upstream_pid" 2>/dev/null || true
        rm -rf "$work"
    }
    trap cleanup EXIT INT TERM
    if [ ! -x "$LARDER_BIN" ]; then
        mkdir -p "$(dirname "$LARDER_BIN")"
        CGO_ENABLED=0 go build -o "$LARDER_BIN" ./cmd/larder
    fi
    mkdir -p "$work/upstream/files" "$work/data"
    printf 'offline drill payload\n' > "$work/upstream/files/a"
    printf '/files/a\n' > "$work/urls.txt"
    upstream_port=$(free_port)
    public_port=$(free_port)
    admin_port=$(free_port)
    (cd "$work/upstream" && python3 -m http.server "$upstream_port" --bind 127.0.0.1 >"$work/upstream.log" 2>&1) &
    upstream_pid=$!
    cat > "$work/larder.yaml" <<YAML
version: 1
data_dir: $work/data
listen: 127.0.0.1:$public_port
admin:
  listen: 127.0.0.1:$admin_port
storage:
  max_bytes: 1GiB
  min_free_bytes: 0
  hard_free_bytes: 0
repos:
  - name: generic
    kind: generic
    mount: /generic
    upstreams:
      - url: http://127.0.0.1:$upstream_port
    rules:
      - match: '^/files/.*'
        class: immutable
YAML
    "$LARDER_BIN" -c "$work/larder.yaml" serve >"$work/server.log" 2>&1 &
    server_pid=$!
    wait_ready "http://127.0.0.1:$admin_port/readyz"
    "$LARDER_BIN" -c "$work/larder.yaml" warm --server "http://127.0.0.1:$public_port" \
        --admin "http://127.0.0.1:$admin_port" --url-list "$work/urls.txt" --repo generic --json > "$work/online.json"
    grep -q '"failed": 0' "$work/online.json"
    grep -q '"fetched": 1' "$work/online.json"
    "$LARDER_BIN" -c "$work/larder.yaml" offline on >/dev/null
    kill "$upstream_pid" 2>/dev/null || true
    upstream_pid=
    "$LARDER_BIN" -c "$work/larder.yaml" warm --server "http://127.0.0.1:$public_port" \
        --admin "http://127.0.0.1:$admin_port" --url-list "$work/urls.txt" --repo generic --json > "$work/offline.json"
    grep -q '"failed": 0' "$work/offline.json"
    grep -q '"fetched": 0' "$work/offline.json"
    if grep -q 'OFFLINE-MISS' "$work/server.log"; then
        echo "offline drill saw an OFFLINE-MISS" >&2
        exit 1
    fi
    "$LARDER_BIN" -c "$work/larder.yaml" offline off >/dev/null
    echo "offline drill passed"
}

case "${1:-}" in
--self-test)
    self_test
    ;;
"")
    echo "usage: offline-drill.sh --self-test | CONFIG WARM_ARGS..." >&2
    exit 2
    ;;
*)
    config=$1
    shift
    if [ ! -x "$LARDER_BIN" ]; then
        echo "larder binary not found: $LARDER_BIN" >&2
        exit 2
    fi
    "$LARDER_BIN" -c "$config" warm "$@" >/dev/null
    "$LARDER_BIN" -c "$config" offline on >/dev/null
    "$LARDER_BIN" -c "$config" warm "$@" --json > /tmp/larder-offline-drill.json
    grep -q '"failed": 0' /tmp/larder-offline-drill.json
    "$LARDER_BIN" -c "$config" offline off >/dev/null
    echo "offline drill passed"
    ;;
esac
