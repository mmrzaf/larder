#!/usr/bin/env bash
# Release acceptance: real npm, pip, apt and apk clients against a real Larder binary and
# the real upstream registries.
#
# Every suite must run. A missing tool, an unreachable registry or a failed
# assertion stops the script with a non-zero status; nothing is skipped.
#
#   scripts/acceptance.sh                 all suites
#   ACCEPT_SUITES=npm scripts/acceptance.sh
#   ACCEPT_DISTROS="debian:bookworm" ACCEPT_SUITES=apt scripts/acceptance.sh
#
# Suites: npm, apt, pypi (pip), alpine (apk), crash (kill -9 during an install), diskfull (Larder on a
# 8 MiB filesystem). Requires go, node, npm, docker and curl, plus network
# access to registry.npmjs.org, pypi.org, files.pythonhosted.org, dl-cdn.alpinelinux.org, deb.debian.org, security.ubuntu.com,
# archive.ubuntu.com and Docker Hub. Set ACCEPT_UPSTREAM_PROXY=env when Larder
# must reach them through HTTPS_PROXY.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SUITES="${ACCEPT_SUITES:-npm apt pypi alpine crash diskfull}"
ALPINES="${ACCEPT_ALPINES:-alpine:3.20 alpine:3.22}"
DISTROS="${ACCEPT_DISTROS:-debian:bookworm debian:trixie ubuntu:22.04 ubuntu:24.04}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/larder-acceptance.XXXXXX")"
CHECKS=0
LARDER_PID=""
ACC_VOLUME=""
PORT=""
ADMIN=""

log() { printf '\n== %s\n' "$*"; }
pass() { CHECKS=$((CHECKS + 1)); printf '   ok   %s\n' "$*"; }
die() {
  printf '\nFAIL: %s\n' "$*" >&2
  if [[ -f "$WORK/larder.log" ]]; then
    printf -- '--- last Larder log lines\n' >&2
    tail -n 25 "$WORK/larder.log" >&2 || true
  fi
  exit 1
}

cleanup() {
  local status=$?
  stop_larder >/dev/null 2>&1 || true
  docker ps -aq --filter "label=larder-acceptance=$$" | xargs -r docker rm -f >/dev/null 2>&1 || true
  if [[ -n $ACC_VOLUME ]]; then
    docker volume rm -f "$ACC_VOLUME" >/dev/null 2>&1 || true
  fi
  chmod -R u+w "$WORK" 2>/dev/null || true
  if [[ $status -eq 0 ]]; then
    rm -rf "$WORK"
  else
    printf 'work directory kept: %s\n' "$WORK" >&2
  fi
  exit "$status"
}
trap cleanup EXIT

require() {
  local missing=()
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
  done
  if ((${#missing[@]})); then
    die "required tools are missing: ${missing[*]}"
  fi
}

want_suite() { [[ " $SUITES " == *" $1 "* ]]; }

port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

pick_ports() {
  local candidate
  while :; do
    candidate=$((20000 + RANDOM % 30000))
    if port_free "$candidate" && port_free $((candidate + 1)); then
      PORT=$candidate
      ADMIN=$((candidate + 1))
      return
    fi
  done
}

# ---------------------------------------------------------------- Larder

DEAD="http://127.0.0.1:9"

# write_config <live|dead> [extra top-level yaml]
write_config() {
  local mode=$1 extra=${2:-} data=${3:-$WORK/data}
  local npm=https://registry.npmjs.org/
  local deb=https://deb.debian.org/debian
  local debsec=https://deb.debian.org/debian-security
  local ubu=https://archive.ubuntu.com/ubuntu
  local ubusec=https://security.ubuntu.com/ubuntu
  local pypi=https://pypi.org
  local alpine=https://dl-cdn.alpinelinux.org/alpine
  if [[ $mode == dead ]]; then
    pypi=$DEAD alpine=$DEAD/alpine
    npm=$DEAD/ deb=$DEAD/debian debsec=$DEAD/debian-security
    ubu=$DEAD/ubuntu ubusec=$DEAD/ubuntu-security
  fi
  cat >"$WORK/larder.yaml" <<EOF
version: 1
data_dir: $data
listen: 127.0.0.1:$PORT
admin:
  listen: 127.0.0.1:$ADMIN
upstream_proxy: ${ACCEPT_UPSTREAM_PROXY:-direct}
log:
  level: info
  sample_hits: 1
timeouts:
  connect: 30s
  tls_handshake: 30s
  revalidate_total: 15s
$extra
repos:
  - name: npm
    kind: npm
    mount: /npm
    fresh_ttl: 1s
    upstreams:
      - url: $npm
  - name: pypi
    kind: pypi
    mount: /pypi
    fresh_ttl: 1s
    files_hosts: [files.pythonhosted.org]
    upstreams:
      - url: $pypi
  - name: alpine
    kind: alpine
    mount: /alpine
    upstreams:
      - url: $alpine
  - name: apt-debian
    kind: apt
    mount: /apt/debian
    upstreams:
      - url: $deb
  - name: apt-debian-security
    kind: apt
    mount: /apt/debian-security
    upstreams:
      - url: $debsec
  - name: apt-ubuntu
    kind: apt
    mount: /apt/ubuntu
    upstreams:
      - url: $ubu
  - name: apt-ubuntu-security
    kind: apt
    mount: /apt/ubuntu-security
    upstreams:
      - url: $ubusec
EOF
}

build_larder() {
  log "building larder"
  (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$WORK/larder" ./cmd/larder)
  "$WORK/larder" version >/dev/null
  pass "binary builds and runs"
}

wait_ready() {
  local i
  for i in $(seq 1 150); do
    if curl -fsS "http://127.0.0.1:$ADMIN/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  die "larder did not become ready"
}

# start_larder <live|dead> [extra yaml] [data dir]
start_larder() {
  local mode=$1 extra=${2:-} data=${3:-$WORK/data}
  [[ -n $PORT ]] || pick_ports
  write_config "$mode" "$extra" "$data"
  "$WORK/larder" -c "$WORK/larder.yaml" config check >/dev/null || die "configuration is invalid"
  "$WORK/larder" -c "$WORK/larder.yaml" serve >>"$WORK/larder.log" 2>&1 &
  LARDER_PID=$!
  wait_ready
}

# stop_larder verifies that a graceful stop exits with status 0.
stop_larder() {
  [[ -n $LARDER_PID ]] || return 0
  local pid=$LARDER_PID
  LARDER_PID=""
  kill -TERM "$pid" 2>/dev/null || return 0
  local status=0
  wait "$pid" 2>/dev/null || status=$?
  [[ $status -eq 0 ]] || die "larder exited with status $status after SIGTERM"
}

restart_larder() {
  stop_larder
  start_larder "$@"
}

kill_larder_hard() {
  [[ -n $LARDER_PID ]] || return 0
  kill -KILL "$LARDER_PID" 2>/dev/null || true
  wait "$LARDER_PID" 2>/dev/null || true
  LARDER_PID=""
}

mark() { wc -l <"$WORK/larder.log" | tr -d ' '; }

# since <mark>: request lines logged after the mark
since() { tail -n +$(($1 + 1)) "$WORK/larder.log" | grep -F '"msg":"request"' || true; }

count_lines() { grep -c . || true; }

# metric <name-with-labels-regex>: sum of matching samples from the admin API
metric() {
  curl -fsS "http://127.0.0.1:$ADMIN/metrics" |
    awk -v pat="$1" '$0 ~ pat && $0 !~ /^#/ { sum += $NF } END { printf "%d\n", sum }'
}

expect_ge() {
  local name=$1 got=$2 want=$3
  [[ $got -ge $want ]] || die "$name: got $got, want at least $want"
  pass "$name ($got)"
}

expect_eq() {
  local name=$1 got=$2 want=$3
  [[ $got -eq $want ]] || die "$name: got $got, want $want"
  pass "$name ($got)"
}

# ------------------------------------------------------------------- npm

NPM_DIR="$WORK/npm"

# npm_client <dir> <cache> <args...>: npm that can only reach Larder. Any
# attempt to contact a registry directly goes to a dead proxy and fails.
npm_client() {
  local dir=$1 cache=$2
  shift 2
  (
    cd "$dir"
    export npm_config_registry="http://127.0.0.1:$PORT/npm/"
    export npm_config_cache="$cache"
    export npm_config_fund=false
    export npm_config_update_notifier=false
    export npm_config_fetch_retries=1
    export npm_config_fetch_retry_mintimeout=200
    export npm_config_fetch_retry_maxtimeout=500
    export HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 NO_PROXY=127.0.0.1
    npm "$@"
  )
}

tree_digest() {
  (cd "$1/node_modules" && find . -type f ! -name '.package-lock.json' -print0 | sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)
}

npm_prepare_project() {
  mkdir -p "$NPM_DIR/app" "$NPM_DIR/vuln"
  cat >"$NPM_DIR/app/package.json" <<'EOF'
{
  "name": "larder-acceptance-app",
  "version": "1.0.0",
  "private": true,
  "dependencies": {
    "express": "4.19.2",
    "@types/node": "20.11.30",
    "left-pad": "1.3.0"
  }
}
EOF
  cat >"$NPM_DIR/vuln/package.json" <<'EOF'
{
  "name": "larder-acceptance-vuln",
  "version": "1.0.0",
  "private": true,
  "dependencies": { "lodash": "4.17.15" }
}
EOF
  log "npm: generating lockfiles against the public registry (not through Larder)"
  for project in app vuln; do
    (
      cd "$NPM_DIR/$project"
      npm install --package-lock-only --ignore-scripts --no-audit --no-fund \
        --registry=https://registry.npmjs.org/ --cache="$NPM_DIR/lock-cache" >/dev/null
    ) || die "could not generate the $project lockfile from the public registry"
  done
  grep -q '"node_modules/@types/node"' "$NPM_DIR/app/package-lock.json" || die "lockfile misses the scoped package"
  pass "lockfiles generated"
}

npm_ci_into() { # <name> <cache>
  rm -rf "$NPM_DIR/$1"
  mkdir -p "$NPM_DIR/$1"
  cp "$NPM_DIR/app/package.json" "$NPM_DIR/app/package-lock.json" "$NPM_DIR/$1/"
  npm_client "$NPM_DIR/$1" "$2" ci --ignore-scripts --no-audit
}

suite_npm() {
  log "npm (N12)"
  require node npm curl
  npm_prepare_project

  log "npm: cold npm ci through Larder (clean data directory, clean npm cache)"
  rm -rf "$WORK/data"
  start_larder live
  local before
  before=$(mark)
  npm_ci_into cold "$NPM_DIR/cache-cold" >"$NPM_DIR/cold.out" 2>&1 || { cat "$NPM_DIR/cold.out"; die "npm ci through Larder failed"; }
  (cd "$NPM_DIR/cold" && node -e "require('express'); require('left-pad')") || die "installed packages do not load"
  [[ -d "$NPM_DIR/cold/node_modules/@types/node" ]] || die "scoped package was not installed"
  expect_ge "npm tarballs fetched from upstream" "$(since "$before" | grep -F '"repo":"npm"' | grep -F '.tgz' | grep -cF '"cache":"MISS"')" 10
  [[ "$(since "$before" | grep -F '"repo":"npm"' | grep -cF '"status":5')" -eq 0 ]] || die "Larder answered 5xx during the cold install"
  pass "cold npm ci succeeded and traffic passed through Larder"
  # npm refuses a tarball whose integrity differs from the lockfile, so a
  # successful ci also proves Larder delivered the exact upstream bytes.

  log "npm: packument tarball URLs point back at Larder"
  local doc
  doc=$(curl -fsS "http://127.0.0.1:$PORT/npm/left-pad")
  grep -qF "\"tarball\":\"http://127.0.0.1:$PORT/npm/left-pad/-/left-pad-1.3.0.tgz\"" <<<"$doc" || die "tarball URL was not rewritten to the public base"
  ! grep -qF 'registry.npmjs.org' <<<"$doc" || die "packument still references the upstream registry"
  doc=$(curl -fsS -H 'X-Forwarded-Proto: https' -H 'X-Forwarded-Host: mirror.test' "http://127.0.0.1:$PORT/npm/left-pad")
  grep -qF 'http://127.0.0.1:' <<<"$doc" || die "forwarded headers are honoured although trust_forwarded is off"
  pass "tarball rewriting follows the request host"

  log "npm: concurrent installs share one download per tarball"
  stop_larder
  rm -rf "$WORK/data"
  start_larder live
  before=$(mark)
  local pids=() i
  for i in 1 2 3 4 5 6; do
    (npm_ci_into "par$i" "$NPM_DIR/cache-par$i" >"$NPM_DIR/par$i.out" 2>&1) &
    pids+=($!)
  done
  for i in "${!pids[@]}"; do
    wait "${pids[$i]}" || { cat "$NPM_DIR/par$((i + 1)).out"; die "concurrent npm ci #$((i + 1)) failed"; }
  done
  local ref
  ref=$(tree_digest "$NPM_DIR/par1")
  for i in 2 3 4 5 6; do
    [[ "$(tree_digest "$NPM_DIR/par$i")" == "$ref" ]] || die "install #$i differs from install #1"
  done
  pass "six parallel installs produced identical trees"
  local tarballs fetched
  tarballs=$(since "$before" | grep -F '"repo":"npm"' | grep -F '.tgz' | grep -F '"status":200' | grep -o '"path":"[^"]*"' | sort -u | wc -l | tr -d ' ')
  fetched=$(metric 'larder_upstream_requests_total\{repo="npm".*outcome="ok"')
  [[ $fetched -le $((tarballs + 120)) ]] || die "upstream saw $fetched requests for $tarballs distinct tarballs: concurrent downloads were not shared"
  pass "upstream requests ($fetched) stay near the number of distinct objects ($tarballs)"

  log "npm: restart between builds, then reuse from cache"
  restart_larder live
  before=$(mark)
  npm_ci_into warm "$NPM_DIR/cache-warm" >"$NPM_DIR/warm.out" 2>&1 || { cat "$NPM_DIR/warm.out"; die "npm ci after a restart failed"; }
  mkdir -p "$NPM_DIR/meta-warm"
  npm_client "$NPM_DIR/meta-warm" "$NPM_DIR/cache-meta-warm" install --ignore-scripts --no-audit left-pad@1.3.0 >"$NPM_DIR/meta-warm.out" 2>&1 || { cat "$NPM_DIR/meta-warm.out"; die "npm install of a single package failed"; }
  expect_eq "tarballs fetched again after restart" "$(since "$before" | grep -F '"repo":"npm"' | grep -F '.tgz' | grep -cF '"cache":"MISS"')" 0
  expect_ge "tarballs served from cache" "$(since "$before" | grep -F '"repo":"npm"' | grep -F '.tgz' | grep -cF '"cache":"HIT"')" 10

  log "npm: upstream unreachable (Larder restarted with an upstream that refuses connections)"
  restart_larder dead
  before=$(mark)
  npm_ci_into outage "$NPM_DIR/cache-outage" >"$NPM_DIR/outage.out" 2>&1 || { cat "$NPM_DIR/outage.out"; die "npm ci with the upstream down failed although everything was cached"; }
  [[ "$(tree_digest "$NPM_DIR/outage")" == "$ref" ]] || die "install during the outage differs from the original"
  pass "cached dependencies install while the upstream is down"
  mkdir -p "$NPM_DIR/meta-outage"
  npm_client "$NPM_DIR/meta-outage" "$NPM_DIR/cache-meta-outage" install --ignore-scripts --no-audit left-pad@1.3.0 >"$NPM_DIR/meta-outage.out" 2>&1 || { cat "$NPM_DIR/meta-outage.out"; die "resolving a cached package from stale metadata failed"; }
  expect_ge "metadata served stale" "$(since "$before" | grep -F '"repo":"npm"' | grep -F '"cache":"STALE"' | grep -cF '"stale":true')" 1
  if npm_client "$NPM_DIR/outage" "$NPM_DIR/cache-outage2" install --ignore-scripts --no-audit cowsay@1.6.0 >"$NPM_DIR/missing.out" 2>&1; then
    die "installing an uncached package during the outage reported success"
  fi
  pass "an uncached package fails clearly instead of succeeding"

  log "npm audit never reports a clean result it did not obtain"
  restart_larder live
  rm -rf "$NPM_DIR/vuln/node_modules"
  local rc=0
  npm_client "$NPM_DIR/vuln" "$NPM_DIR/cache-vuln" audit >"$NPM_DIR/audit-live.out" 2>&1 || rc=$?
  [[ $rc -ne 0 ]] || { cat "$NPM_DIR/audit-live.out"; die "npm audit exited 0 for a package with known advisories"; }
  grep -qiE 'vulnerabilit' "$NPM_DIR/audit-live.out" || { cat "$NPM_DIR/audit-live.out"; die "npm audit through Larder returned no advisory report"; }
  pass "npm audit through Larder reports advisories"
  restart_larder dead
  rc=0
  npm_client "$NPM_DIR/vuln" "$NPM_DIR/cache-vuln" audit >"$NPM_DIR/audit-dead.out" 2>&1 || rc=$?
  [[ $rc -ne 0 ]] || { cat "$NPM_DIR/audit-dead.out"; die "npm audit exited 0 while the audit endpoint was unreachable"; }
  ! grep -qE 'found 0 vulnerabilities' "$NPM_DIR/audit-dead.out" || die "npm audit claimed a clean result without contacting the advisory service"
  pass "npm audit fails with the upstream down instead of reporting a clean result"

  log "npm: Docker build with the registry passed as a build argument"
  restart_larder live
  require docker
  mkdir -p "$NPM_DIR/docker"
  cp "$NPM_DIR/app/package.json" "$NPM_DIR/app/package-lock.json" "$NPM_DIR/docker/"
  cat >"$NPM_DIR/docker/Dockerfile" <<'EOF'
FROM node:24-bookworm-slim
ARG NPM_CONFIG_REGISTRY
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund && node -e "require('express')"
EOF
  before=$(mark)
  docker build --network=host --no-cache --build-arg "NPM_CONFIG_REGISTRY=http://127.0.0.1:$PORT/npm/" \
    -t larder-acceptance-npm:test "$NPM_DIR/docker" >"$NPM_DIR/docker.out" 2>&1 || { tail -30 "$NPM_DIR/docker.out"; die "docker build through Larder failed"; }
  docker rmi -f larder-acceptance-npm:test >/dev/null 2>&1 || true
  expect_ge "docker build fetched through Larder" "$(since "$before" | grep -F '"repo":"npm"' | grep -F '.tgz' | grep -cF '"status":200')" 10
  stop_larder
}

# ------------------------------------------------------------------- apt

APT_CLIENT="$WORK/apt-client.sh"

write_apt_client() {
  cat >"$APT_CLIENT" <<'EOF'
#!/bin/bash
# Runs inside a Debian or Ubuntu container that can reach only Larder.
set -eu
scenario=$1
base=http://127.0.0.1:$PORT
. /etc/os-release
codename=${VERSION_CODENAME}
rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*

if [ "$ID" = debian ]; then
  keyring=/usr/share/keyrings/debian-archive-keyring.gpg
  archive=$base/apt/debian
  security=$base/apt/debian-security
  components=main
else
  keyring=/usr/share/keyrings/ubuntu-archive-keyring.gpg
  archive=$base/apt/ubuntu
  security=$base/apt/ubuntu-security
  components="main universe"
fi
[ "${WRONG_KEY:-0}" = 1 ] && { : >/tmp/empty.gpg; keyring=/tmp/empty.gpg; }

cat >/etc/apt/sources.list.d/larder.sources <<SRC
Types: deb
URIs: $archive
Suites: $codename ${codename}-updates
Components: $components
Signed-By: $keyring

Types: deb
URIs: $security
Suites: ${codename}-security
Components: $components
Signed-By: $keyring
SRC

cat >/etc/apt/apt.conf.d/99acceptance <<CONF
Acquire::http::Proxy "http://127.0.0.1:9";
Acquire::http::Proxy::127.0.0.1 "DIRECT";
Acquire::Retries "0";
Acquire::Languages "none";
Acquire::http::Timeout "30";
APT::Install-Recommends "false";
CONF
export DEBIAN_FRONTEND=noninteractive

case "$scenario" in
  install)
    apt-get update 2>&1
    apt-get install -y hello jq 2>&1
    hello --version | head -1
    echo '{"a":1}' | jq -e .a >/dev/null
    ;;
  update-only)
    apt-get update 2>&1
    ;;
  uncached)
    apt-get update 2>&1
    apt-get install -y --no-install-recommends cowsay 2>&1
    ;;
  arm64)
    dpkg --add-architecture arm64
    apt-get update 2>&1
    cd /tmp && apt-get download hello:arm64 2>&1
    test "$(dpkg-deb -f /tmp/hello_*_arm64.deb Architecture)" = arm64
    ;;
  *) echo "unknown scenario $scenario" >&2; exit 2 ;;
esac
EOF
  chmod 755 "$APT_CLIENT"
}

# apt_run <image> <scenario> [env assignments...]
apt_run() {
  local image=$1 scenario=$2
  shift 2
  docker run --rm --network host --label "larder-acceptance=$$" \
    -e "PORT=$PORT" "${@/#/-e}" \
    -v "$APT_CLIENT:/apt-client.sh:ro" "$image" /apt-client.sh "$scenario"
}

suite_apt() {
  log "apt (A10)"
  require docker curl
  write_apt_client
  local image tag
  for image in $DISTROS; do
    tag=${image//[:.]/-}
    log "apt: $image"
    docker pull -q "$image" >/dev/null || die "cannot pull $image"
    rm -rf "$WORK/data"
    start_larder live
    local before out
    before=$(mark)
    out="$WORK/apt-$tag-cold.out"
    apt_run "$image" install >"$out" 2>&1 || { tail -40 "$out"; die "$image: apt-get update/install through Larder failed"; }
    ! grep -qE 'Hash Sum mismatch|NO_PUBKEY|is not signed|BADSIG' "$out" || { tail -40 "$out"; die "$image: apt reported an integrity or signature problem"; }
    ! grep -qE '^(W|E):' "$out" || { grep -E '^(W|E):' "$out"; die "$image: apt printed warnings or errors"; }
    pass "$image: update and install verified signatures through Larder"
    expect_ge "$image: by-hash index fetches" "$(since "$before" | grep -cF '/by-hash/')" 1
    expect_ge "$image: package downloads" "$(since "$before" | grep -F '/pool/' | grep -cF '"status":200')" 2
    expect_ge "$image: security and updates suites fetched" "$(since "$before" | grep -cE 'dists/[a-z]+-(security|updates)/')" 2

    log "apt: $image apt keeps enforcing signatures through Larder"
    if apt_run "$image" update-only WRONG_KEY=1 >"$WORK/apt-$tag-wrongkey.out" 2>&1; then
      die "$image: apt-get update succeeded with a keyring that cannot verify the repository"
    fi
    grep -qE 'not signed|NO_PUBKEY|signatures couldn|public key' "$WORK/apt-$tag-wrongkey.out" || { tail "$WORK/apt-$tag-wrongkey.out"; die "$image: unexpected failure mode for an unverifiable repository"; }
    pass "$image: an unverifiable repository is rejected"

    log "apt: $image warm client"
    restart_larder live
    before=$(mark)
    apt_run "$image" install >"$WORK/apt-$tag-warm.out" 2>&1 || { tail -40 "$WORK/apt-$tag-warm.out"; die "$image: warm run failed"; }
    expect_eq "$image: .deb files fetched from upstream again" "$(since "$before" | grep -F '/pool/' | grep -cF '"cache":"MISS"')" 0

    log "apt: $image upstream unreachable"
    restart_larder dead
    before=$(mark)
    apt_run "$image" install >"$WORK/apt-$tag-outage.out" 2>&1 || { tail -40 "$WORK/apt-$tag-outage.out"; die "$image: cached packages did not install while the upstream was down"; }
    pass "$image: update and install work from cache with the upstream down"
    if apt_run "$image" uncached >"$WORK/apt-$tag-uncached.out" 2>&1; then
      die "$image: installing an uncached package during the outage reported success"
    fi
    grep -qE '^E:' "$WORK/apt-$tag-uncached.out" || die "$image: the failure was not reported as an apt error"
    pass "$image: an uncached package fails with an apt error"

    if [[ $image == debian:* ]]; then
      log "apt: $image second architecture"
      restart_larder live
      apt_run "$image" arm64 >"$WORK/apt-$tag-arm64.out" 2>&1 || { tail -30 "$WORK/apt-$tag-arm64.out"; die "$image: arm64 metadata or package fetch failed"; }
      pass "$image: arm64 indexes and packages fetched"
    fi
    stop_larder
  done

  log "apt: concurrent clients on a cold cache"
  rm -rf "$WORK/data"
  start_larder live
  local pids=() i first=${DISTROS%% *}
  for i in 1 2 3 4; do
    (apt_run "$first" install >"$WORK/apt-par$i.out" 2>&1) &
    pids+=($!)
  done
  for i in "${!pids[@]}"; do
    wait "${pids[$i]}" || { tail -30 "$WORK/apt-par$((i + 1)).out"; die "concurrent apt client #$((i + 1)) failed"; }
  done
  ! grep -lE 'Hash Sum mismatch' "$WORK"/apt-par*.out >/dev/null 2>&1 || die "concurrent clients saw a hash mismatch"
  pass "four concurrent clients succeeded without hash mismatches"
  stop_larder
}

# ------------------------------------------------------------------ pypi

PIP_DIR="$WORK/pip"
PYTHON_IMAGE=python:3.12-slim

# pip_run <name> <command>: pip in a clean container that can reach only Larder.
pip_run() {
  local name=$1
  shift
  docker run --rm --network host --label "larder-acceptance=$$" \
    -e HTTP_PROXY=http://127.0.0.1:9 -e HTTPS_PROXY=http://127.0.0.1:9 -e NO_PROXY=127.0.0.1 -e PIP_DISABLE_PIP_VERSION_CHECK=1 \
    -e "PIP_INDEX_URL=http://127.0.0.1:$PORT/pypi/simple/" -e PIP_TRUSTED_HOST=127.0.0.1 \
    -e PIP_RETRIES=0 -e PIP_TIMEOUT=30 \
    -v "$PIP_DIR:/work" "$PYTHON_IMAGE" sh -c "cd /work && $*" >"$PIP_DIR/$name.out" 2>&1
}

PIP_INSTALL="pip install --require-hashes --only-binary=:all: --no-cache-dir -r requirements.txt"

suite_pypi() {
  log "pypi (pip) (P10)"
  require docker curl
  docker pull -q "$PYTHON_IMAGE" >/dev/null || die "cannot pull $PYTHON_IMAGE"
  mkdir -p "$PIP_DIR"
  log "pypi: pinned requirements with hashes, generated against the public index"
  docker run --rm --network host --label "larder-acceptance=$$" -e HTTPS_PROXY -e HTTP_PROXY -e NO_PROXY \
    -e PIP_DISABLE_PIP_VERSION_CHECK=1 -v "$PIP_DIR:/work" "$PYTHON_IMAGE" sh -c '
      cd /work && mkdir -p wheels &&
      printf "requests==2.32.3\ncertifi==2024.8.30\ncharset-normalizer==3.3.2\nidna==3.8\nurllib3==2.2.2\n" > pins.txt &&
      pip download --no-deps --only-binary=:all: -d wheels -r pins.txt >/dev/null &&
      : > requirements.txt &&
      for w in wheels/*.whl; do
        name=$(basename "$w" | cut -d- -f1,2 | sed "s/-/==/" | tr "_" "-")
        echo "$name $(pip hash "$w" | tail -1)" >> requirements.txt
      done' || die "could not generate hashed requirements from the public index"
  [[ $(wc -l <"$PIP_DIR/requirements.txt") -eq 5 ]] || die "unexpected requirements file"
  pass "requirements generated"

  rm -rf "$WORK/data"
  start_larder live
  local before
  before=$(mark)
  pip_run cold "$PIP_INSTALL --target /tmp/t" || { tail -20 "$PIP_DIR/cold.out"; die "pip install through Larder failed"; }
  expect_ge "wheels fetched from upstream" "$(since "$before" | grep -F '"repo":"pypi"' | grep -F '.whl' | grep -cF '"cache":"MISS"')" 5
  [[ "$(since "$before" | grep -F '"repo":"pypi"' | grep -cF '"status":5')" -eq 0 ]] || die "Larder answered 5xx during the cold install"
  pass "hash-checked install succeeded: the wheels are the exact upstream bytes"

  log "pypi: concurrent installs on a cold cache"
  stop_larder
  rm -rf "$WORK/data"
  start_larder live
  local pids=() i
  for i in 1 2 3 4; do
    (pip_run "par$i" "$PIP_INSTALL --target /tmp/t") &
    pids+=($!)
  done
  for i in "${!pids[@]}"; do
    wait "${pids[$i]}" || { tail -20 "$PIP_DIR/par$((i + 1)).out"; die "concurrent pip install #$((i + 1)) failed"; }
  done
  pass "four concurrent installs succeeded"

  log "pypi: restart, then reuse from cache"
  restart_larder live
  before=$(mark)
  pip_run warm "$PIP_INSTALL --target /tmp/t" || { tail -20 "$PIP_DIR/warm.out"; die "install after a restart failed"; }
  expect_eq "wheels fetched again after restart" "$(since "$before" | grep -F '"repo":"pypi"' | grep -F '.whl' | grep -cF '"cache":"MISS"')" 0

  log "pypi: upstream unreachable"
  restart_larder dead
  before=$(mark)
  pip_run outage "$PIP_INSTALL --target /tmp/t" || { tail -20 "$PIP_DIR/outage.out"; die "cached packages did not install while the upstream was down"; }
  pass "cached packages install while the upstream is down"
  expect_ge "indexes served stale" "$(since "$before" | grep -F '"repo":"pypi"' | grep -cF '"stale":true')" 1
  if pip_run missing "pip install --only-binary=:all: --no-cache-dir --target /tmp/m six==1.16.0"; then
    die "installing an uncached package during the outage reported success"
  fi
  pass "an uncached package fails instead of succeeding"
  stop_larder
}

# ---------------------------------------------------------------- alpine

ALPINE_CLIENT="$WORK/apk-client.sh"

write_apk_client() {
  cat >"$ALPINE_CLIENT" <<'CLIENT'
#!/bin/sh
# Runs inside an Alpine container that can reach only Larder.
set -eu
scenario=$1
branch=v$(cut -d. -f1,2 /etc/alpine-release)
printf 'http://127.0.0.1:%s/alpine/%s/main\nhttp://127.0.0.1:%s/alpine/%s/community\n' "$PORT" "$branch" "$PORT" "$branch" > /etc/apk/repositories
export http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 no_proxy=127.0.0.1
case "$scenario" in
  install) apk update && apk add jq && echo '{"a":1}' | jq -e .a >/dev/null ;;
  uncached) apk update && apk add figlet ;;
  wrong-key) rm -f /etc/apk/keys/*; apk update ;;
  *) echo "unknown scenario $scenario" >&2; exit 2 ;;
esac
CLIENT
  chmod 755 "$ALPINE_CLIENT"
}

apk_run() {
  local image=$1 scenario=$2
  docker run --rm --network host --label "larder-acceptance=$$" -e "PORT=$PORT" \
    -v "$ALPINE_CLIENT:/apk-client.sh:ro" "$image" /apk-client.sh "$scenario"
}

suite_alpine() {
  log "alpine (apk) (K04)"
  require docker curl
  write_apk_client
  local image tag before out
  for image in $ALPINES; do
    tag=${image//[:.]/-}
    log "alpine: $image"
    docker pull -q "$image" >/dev/null || die "cannot pull $image"
    rm -rf "$WORK/data"
    start_larder live
    before=$(mark)
    out="$WORK/apk-$tag-cold.out"
    apk_run "$image" install >"$out" 2>&1 || { tail -30 "$out"; die "$image: apk update/add through Larder failed"; }
    ! grep -qE 'UNTRUSTED|BAD signature|ERROR' "$out" || { tail -30 "$out"; die "$image: apk reported a signature problem"; }
    expect_ge "$image: indexes fetched" "$(since "$before" | grep -F '"repo":"alpine"' | grep -F 'APKINDEX' | grep -cF '"status":200')" 2
    expect_ge "$image: packages fetched" "$(since "$before" | grep -F '"repo":"alpine"' | grep -F '.apk' | grep -cF '"cache":"MISS"')" 2

    if apk_run "$image" wrong-key >"$WORK/apk-$tag-wrongkey.out" 2>&1; then
      die "$image: apk update succeeded without any trusted key"
    fi
    grep -qiE 'UNTRUSTED|signature' "$WORK/apk-$tag-wrongkey.out" || { tail "$WORK/apk-$tag-wrongkey.out"; die "$image: unexpected failure for an unverifiable index"; }
    pass "$image: an index that cannot be verified is rejected"

    restart_larder live
    before=$(mark)
    apk_run "$image" install >"$WORK/apk-$tag-warm.out" 2>&1 || { tail -30 "$WORK/apk-$tag-warm.out"; die "$image: warm run failed"; }
    expect_eq "$image: packages fetched again" "$(since "$before" | grep -F '"repo":"alpine"' | grep -F '.apk' | grep -cF '"cache":"MISS"')" 0

    restart_larder dead
    apk_run "$image" install >"$WORK/apk-$tag-outage.out" 2>&1 || { tail -30 "$WORK/apk-$tag-outage.out"; die "$image: cached packages did not install with the upstream down"; }
    pass "$image: update and add work from cache with the upstream down"
    if apk_run "$image" uncached >"$WORK/apk-$tag-uncached.out" 2>&1; then
      die "$image: adding an uncached package during the outage reported success"
    fi
    pass "$image: an uncached package fails"
    stop_larder
  done

  log "alpine: concurrent clients on a cold cache"
  rm -rf "$WORK/data"
  start_larder live
  local pids=() i first=${ALPINES%% *}
  for i in 1 2 3 4; do
    (apk_run "$first" install >"$WORK/apk-par$i.out" 2>&1) &
    pids+=($!)
  done
  for i in "${!pids[@]}"; do
    wait "${pids[$i]}" || { tail -20 "$WORK/apk-par$((i + 1)).out"; die "concurrent apk client #$((i + 1)) failed"; }
  done
  pass "four concurrent clients succeeded"
  stop_larder
}

# ----------------------------------------------------------------- crash

suite_crash() {
  log "crash: kill -9 during an install"
  require node npm curl
  [[ -d "$NPM_DIR/app" ]] || npm_prepare_project
  rm -rf "$WORK/data"
  start_larder live
  rm -rf "$NPM_DIR/crash" "$NPM_DIR/cache-crash"
  mkdir -p "$NPM_DIR/crash"
  cp "$NPM_DIR/app/package.json" "$NPM_DIR/app/package-lock.json" "$NPM_DIR/crash/"
  (npm_client "$NPM_DIR/crash" "$NPM_DIR/cache-crash" ci --ignore-scripts --no-audit >"$NPM_DIR/crash.out" 2>&1) &
  local client=$!
  sleep 1.5
  kill_larder_hard
  wait "$client" || true
  pass "larder killed while a client was downloading"

  start_larder live
  rm -rf "$NPM_DIR/cache-crash2"
  npm_ci_into crash2 "$NPM_DIR/cache-crash2" >"$NPM_DIR/crash2.out" 2>&1 || { cat "$NPM_DIR/crash2.out"; die "install after a crash failed"; }
  pass "install after the crash succeeded"
  stop_larder
  local report
  report=$("$WORK/larder" -c "$WORK/larder.yaml" verify 2>&1) || { printf '%s\n' "$report"; die "stored blobs are corrupt after a crash"; }
  pass "every stored blob still matches its digest"
  report=$("$WORK/larder" -c "$WORK/larder.yaml" reindex 2>&1) || { printf '%s\n' "$report"; die "index and blob store disagree after a crash"; }
  pass "index and blob store agree"
  [[ -z "$(ls -A "$WORK/data/tmp" 2>/dev/null)" ]] || die "temporary files remain after a clean restart"
  pass "no temporary files remain"
}

# -------------------------------------------------------------- disk full

suite_diskfull() {
  log "diskfull: Larder on an 8 MiB filesystem"
  require docker curl
  local image=${DISTROS%% *}
  # The container that runs Larder needs CA certificates, which the release
  # image has and plain distribution images do not.
  local larder_image=gcr.io/distroless/static-debian12
  write_apt_client
  docker pull -q "$image" >/dev/null || die "cannot pull $image"
  docker pull -q "$larder_image" >/dev/null || die "cannot pull $larder_image"
  pick_ports
  local extra=$'storage:\n  max_bytes: 6MiB\n  min_free_bytes: 2MiB\n  hard_free_bytes: 1MiB'
  write_config live "$extra" /data
  cp "$WORK/larder" "$WORK/larder-linux"
  local container="larder-acceptance-$$" volume="larder-acceptance-$$"
  ACC_VOLUME=$volume
  # A size-limited volume that survives container restarts.
  docker volume create --driver local --opt type=tmpfs --opt device=tmpfs \
    --opt o=size=8m,mode=1777 "$volume" >/dev/null || die "cannot create a size-limited volume"

  larder_container() {
    docker run -d --name "$container" --label "larder-acceptance=$$" --network host \
      --user 10001:10001 -e HTTPS_PROXY -e HTTP_PROXY -e NO_PROXY \
      -v "$volume:/data" \
      -v "$WORK/larder-linux:/larder:ro" -v "$WORK/larder.yaml:/etc/larder.yaml:ro" \
      "$larder_image" /larder -c /etc/larder.yaml serve >/dev/null
    local i
    for i in $(seq 1 100); do
      curl -fsS "http://127.0.0.1:$ADMIN/healthz" >/dev/null 2>&1 && return 0
      sleep 0.2
    done
    docker logs "$container" 2>&1 | tail -20
    return 1
  }

  larder_container || die "larder did not start on the small filesystem"
  apt_run "$image" install >"$WORK/apt-full.out" 2>&1 || { tail -30 "$WORK/apt-full.out"; docker logs "$container" 2>&1 | tail -15 | cut -c1-250; die "client failed while Larder's disk was full"; }
  pass "client finished although the cache filesystem is smaller than what it downloaded"
  curl -fsS "http://127.0.0.1:$ADMIN/healthz" >/dev/null || die "larder stopped answering under disk pressure"
  docker logs "$container" 2>&1 | grep -qE 'bypass_start|storage_reservation_refused|fetch_local_failure' || die "larder did not report the shortage"
  pass "larder stayed up and reported the shortage"
  docker stop -t 30 "$container" >/dev/null
  [[ "$(docker inspect --format '{{.State.ExitCode}}' "$container")" == 0 ]] || die "larder did not stop cleanly with a full disk"
  docker rm "$container" >/dev/null
  larder_container || die "larder does not start again on the nearly full filesystem"
  pass "larder stops cleanly and starts again on the nearly full filesystem"
  apt_run "$image" install >"$WORK/apt-full2.out" 2>&1 || { tail -30 "$WORK/apt-full2.out"; die "client failed after the restart on a full disk"; }
  pass "clients are still served after the restart"
  docker rm -f "$container" >/dev/null 2>&1 || true
  PORT=""
}

# ------------------------------------------------------------------ main

printf 'Larder acceptance. Suites: %s\n' "$SUITES"
require go curl
build_larder
for suite in $SUITES; do
  case "$suite" in
    npm) suite_npm ;;
    apt) suite_apt ;;
    pypi) suite_pypi ;;
    alpine) suite_alpine ;;
    crash) suite_crash ;;
    diskfull) suite_diskfull ;;
    *) die "unknown suite $suite" ;;
  esac
done
printf '\nPASS: %d checks, no skips\n' "$CHECKS"
