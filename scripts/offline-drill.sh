#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
LARDER_BIN=${LARDER_BIN:-$PWD/bin/larder}

free_ports() {
	python3 - "$1" <<'PY'
import socket, sys
sockets = []
for _ in range(int(sys.argv[1])):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    sockets.append(s)
for s in sockets:
    print(s.getsockname()[1])
    s.close()
PY
}

wait_upstream() {
	url=$1
	attempts=0
	while [ "$attempts" -lt 100 ]; do
		if python3 -c 'import sys, urllib.request; urllib.request.urlopen(sys.argv[1], timeout=1).read()' "$url" >/dev/null 2>&1; then
			return 0
		fi
		attempts=$((attempts + 1))
		sleep 0.1
	done
	return 1
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
	printf 'offline drill payload\n' >"$work/upstream/files/a"
	printf '/files/a\n' >"$work/urls.txt"
	# Hold all three sockets until every port is known so no two listeners
	# in this drill can be assigned the same ephemeral port.
	set -- $(free_ports 3)
	upstream_port=$1
	public_port=$2
	admin_port=$3
	(cd "$work/upstream" && python3 -m http.server "$upstream_port" --bind 127.0.0.1 >"$work/upstream.log" 2>&1) &
	upstream_pid=$!
	# The first warm must not outrun the upstream process: a refused
	# connection there fails the drill before any object is cached.
	if ! wait_upstream "http://127.0.0.1:$upstream_port/files/a"; then
		echo "offline drill upstream did not become ready" >&2
		exit 1
	fi
	cat >"$work/larder.yaml" <<YAML
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
		--admin "http://127.0.0.1:$admin_port" --url-list "$work/urls.txt" --repo generic --json >"$work/online.json"
	grep -q '"failed": 0' "$work/online.json"
	grep -q '"fetched": 1' "$work/online.json"
	"$LARDER_BIN" -c "$work/larder.yaml" offline on >/dev/null
	kill "$upstream_pid" 2>/dev/null || true
	upstream_pid=
	"$LARDER_BIN" -c "$work/larder.yaml" warm --server "http://127.0.0.1:$public_port" \
		--admin "http://127.0.0.1:$admin_port" --url-list "$work/urls.txt" --repo generic --json >"$work/offline.json"
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
	"$LARDER_BIN" -c "$config" warm "$@" --json >/tmp/larder-offline-drill.json
	grep -q '"failed": 0' /tmp/larder-offline-drill.json
	"$LARDER_BIN" -c "$config" offline off >/dev/null
	echo "offline drill passed"
	;;
esac
