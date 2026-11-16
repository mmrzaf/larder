#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

echo "== gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
    printf '%s\n' "$unformatted"
    exit 1
fi

echo "== vet"
go vet ./...

echo "== build"
CGO_ENABLED=0 go build ./...

echo "== test"
go test ./...

if command -v gcc >/dev/null 2>&1 || command -v cc >/dev/null 2>&1; then
    echo "== race"
    go test -race ./...
else
    echo "== race skipped: no C toolchain"
fi

echo "== coverage"
threshold_packages="cache store index storage upstream kinds/gokind kinds/mavenkind kinds/npmkind kinds/pypikind kinds/cratekind kinds/rustupkind kinds/aptkind kinds/alpinekind kinds/generickind"
for package in $threshold_packages; do
    output=$(go test -cover "./internal/$package")
    percent=$(printf '%s\n' "$output" | sed -n 's/.*coverage: \([0-9][0-9.]*\)%.*/\1/p' | tail -1)
    if [ -z "$percent" ]; then
        echo "$package: coverage not reported"
        exit 1
    fi
    awk -v name="$package" -v value="$percent" 'BEGIN { if (value + 0 < 80) { printf "%s coverage %s%% below 80%%\n", name, value; exit 1 } }'
    echo "$package: $percent%"
done
go test -cover ./... >/dev/null

echo "== module verification"
go mod verify

echo "== hygiene"
./scripts/hygiene.sh

echo "== consistency"
go test -count=1 ./internal/consistency
