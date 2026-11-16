#!/usr/bin/env bash
set -euo pipefail

if [[ "${LARDER_E2E:-0}" != "1" ]]; then
  echo "real client checks are opt-in; set LARDER_E2E=1 to run"
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
SERVER_PID=""
PUBLIC_PORT=""
ADMIN_PORT=""

cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  chmod -R u+w "$WORK" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

free_ports() {
  python3 - <<'PY'
import socket
sockets = []
for _ in range(2):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    sockets.append(s)
print(sockets[0].getsockname()[1], sockets[1].getsockname()[1])
PY
}

read -r PUBLIC_PORT ADMIN_PORT <<<"$(free_ports)"

cat >"$WORK/larder.yaml" <<EOF
version: 1
data_dir: $WORK/data
listen: 127.0.0.1:$PUBLIC_PORT
admin:
  listen: 127.0.0.1:$ADMIN_PORT
log:
  level: warn
repos:
  - name: go
    kind: go
    mount: /go
    upstreams:
      - url: https://proxy.golang.org
  - name: maven-central
    kind: maven
    mount: /maven/central
    upstreams:
      - url: https://repo.maven.apache.org/maven2
  - name: pypi
    kind: pypi
    mount: /pypi
    upstreams:
      - url: https://pypi.org
    files_hosts: [files.pythonhosted.org]
  - name: crates
    kind: crates
    mount: /crates
    upstreams:
      - url: https://index.crates.io
    dl_upstreams:
      - https://static.crates.io/crates/{crate}/{crate}-{version}.crate
  - name: rustup
    kind: rustup
    mount: /rustup
    upstreams:
      - url: https://static.rust-lang.org
EOF

(cd "$ROOT" && go build -o "$WORK/larder" ./cmd/larder)
"$WORK/larder" -c "$WORK/larder.yaml" serve >"$WORK/larder.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:$ADMIN_PORT/readyz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
if ! curl -fsS "http://127.0.0.1:$ADMIN_PORT/readyz" >/dev/null 2>&1; then
  echo "larder did not become ready" >&2
  cat "$WORK/larder.log" >&2
  exit 1
fi

offline() {
  curl -fsS -X POST "http://127.0.0.1:$ADMIN_PORT/_larder/v1/offline" \
    -H 'Content-Type: application/json' -d '{"offline":true}' >/dev/null
}

online() {
  curl -fsS -X POST "http://127.0.0.1:$ADMIN_PORT/_larder/v1/offline" \
    -H 'Content-Type: application/json' -d '{"offline":false}' >/dev/null
}

e2e_go() {
  if ! command -v go >/dev/null 2>&1; then
    echo "G09 skipped: go is not installed"
    return 0
  fi
  echo "G09: go mod download online then offline"
  local dir="$WORK/go-client"
  mkdir -p "$dir"
  (
    cd "$dir"
    export GOPROXY="http://127.0.0.1:$PUBLIC_PORT/go,off"
    export GOSUMDB=off
    go mod init example.com/larder-e2e >/dev/null
    GOMODCACHE="$dir/modcache-online" go mod download golang.org/x/text@v0.14.0
    offline
    GOMODCACHE="$dir/modcache-offline" go mod download golang.org/x/text@v0.14.0
    online
  )
}

e2e_maven() {
  if ! command -v mvn >/dev/null 2>&1; then
    echo "M08 (maven) skipped: mvn is not installed"
    return 0
  fi
  echo "M08: mvn dependency:get online then offline"
  local dir="$WORK/maven-client"
  mkdir -p "$dir"
  cat >"$dir/settings.xml" <<EOF
<settings>
  <mirrors>
    <mirror>
      <id>larder</id>
      <mirrorOf>*</mirrorOf>
      <url>http://127.0.0.1:$PUBLIC_PORT/maven/central</url>
    </mirror>
  </mirrors>
</settings>
EOF
  (
    cd "$dir"
    mvn -B -ntp -q -s settings.xml -Dmaven.repo.local="$dir/repository" \
      dependency:get -Dartifact=junit:junit:4.13.2 -Dtransitive=false
    offline
    rm -rf "$dir/repository"
    mvn -B -ntp -q -s settings.xml -Dmaven.repo.local="$dir/repository" \
      dependency:get -Dartifact=junit:junit:4.13.2 -Dtransitive=false
    online
  )
}

e2e_gradle() {
  if ! command -v gradle >/dev/null 2>&1; then
    echo "M08 (gradle) skipped: gradle is not installed"
    return 0
  fi
  echo "M08: gradle resolution online then offline"
  local dir="$WORK/gradle-client"
  mkdir -p "$dir"
  cat >"$dir/build.gradle" <<'EOF'
configurations { e2e }
dependencies { e2e 'junit:junit:4.13.2' }
EOF
  cat >"$dir/init.gradle" <<EOF
allprojects {
  repositories {
    maven {
      url "http://127.0.0.1:$PUBLIC_PORT/maven/central"
      allowInsecureProtocol = true
    }
  }
}
EOF
  (
    cd "$dir"
    gradle -g "$dir/gradle-home" -I init.gradle --no-daemon -q dependencies --configuration e2e
    offline
    rm -rf "$dir/gradle-home"
    gradle -g "$dir/gradle-home" -I init.gradle --no-daemon -q dependencies --configuration e2e
    online
  )
}

e2e_pip() {
  if ! command -v python3 >/dev/null 2>&1 || ! python3 -m pip --version >/dev/null 2>&1; then
    echo "P10 skipped: pip is not installed"
    return 0
  fi
  echo "P10: pip download online then offline"
  local dir="$WORK/pip-client"
  mkdir -p "$dir/online" "$dir/offline" "$dir/cache-online" "$dir/cache-offline"
  (
    export PIP_INDEX_URL="http://127.0.0.1:$PUBLIC_PORT/pypi/simple/"
    export PIP_TRUSTED_HOST="127.0.0.1"
    export PIP_CACHE_DIR="$dir/cache-online"
    python3 -m pip download --no-deps --dest "$dir/online" six==1.16.0 >/dev/null
    offline
    export PIP_CACHE_DIR="$dir/cache-offline"
    python3 -m pip download --no-deps --dest "$dir/offline" six==1.16.0 >/dev/null
    online
  )
}

e2e_cargo() {
  if ! command -v cargo >/dev/null 2>&1; then
    echo "R08 skipped: cargo is not installed"
    return 0
  fi
  echo "R08: cargo fetch online then offline"
  local dir="$WORK/cargo-client"
  mkdir -p "$dir/project/src" "$dir/home-online" "$dir/home-offline"
  cat >"$dir/project/Cargo.toml" <<'EOF'
[package]
name = "larder-e2e"
version = "0.1.0"
edition = "2021"

[dependencies]
serde = "=1.0.195"
EOF
  : >"$dir/project/src/lib.rs"
  cat >"$dir/config.toml" <<EOF
[source.crates-io]
replace-with = "larder"

[source.larder]
registry = "sparse+http://127.0.0.1:$PUBLIC_PORT/crates/index/"
EOF
  cp "$dir/config.toml" "$dir/home-online/config.toml"
  cp "$dir/config.toml" "$dir/home-offline/config.toml"
  (cd "$dir/project" && CARGO_HOME="$dir/home-online" cargo fetch)
  offline
  (cd "$dir/project" && CARGO_HOME="$dir/home-offline" cargo fetch)
  online
}

e2e_rustup() {
  if ! command -v docker >/dev/null 2>&1; then
    echo "R09 skipped: docker is not installed"
    return 0
  fi
  echo "R09: rustup toolchain install online then offline"
  local dir="$WORK/rustup-client"
  mkdir -p "$dir/online" "$dir/offline"
  docker run --rm --network host -v "$dir/online:/state" \
    -e RUSTUP_HOME=/state/rustup -e CARGO_HOME=/state/cargo \
    -e RUSTUP_DIST_SERVER="http://127.0.0.1:$PUBLIC_PORT/rustup" \
    -e RUSTUP_UPDATE_ROOT="http://127.0.0.1:$PUBLIC_PORT/rustup/rustup" \
    rust:1-bookworm bash -c 'rustup toolchain install stable --profile minimal'
  offline
  docker run --rm --network host -v "$dir/offline:/state" \
    -e RUSTUP_HOME=/state/rustup -e CARGO_HOME=/state/cargo \
    -e RUSTUP_DIST_SERVER="http://127.0.0.1:$PUBLIC_PORT/rustup" \
    -e RUSTUP_UPDATE_ROOT="http://127.0.0.1:$PUBLIC_PORT/rustup/rustup" \
    rust:1-bookworm bash -c 'rustup toolchain install stable --profile minimal'
  online
}

e2e_go
e2e_maven
e2e_gradle
e2e_pip
e2e_cargo
e2e_rustup
