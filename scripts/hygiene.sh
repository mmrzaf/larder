#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

pattern='(^|[^[:alnum:]_])(phase|per the plan|as discussed|added in review|in this change|step[[:space:]]+[0-9]|iteration[[:space:]]+[0-9]|TODO|FIXME)([^[:alnum:]_]|$)'
matches=$(find . -type f \
    ! -path './.git/*' \
    ! -path './bin/*' \
    ! -path './dist/*' \
    ! -name 'AGENTS.md' \
    ! -path './scripts/hygiene.sh' \
    ! -name 'larder' \
    ! -name 'coverage.out' \
    -print0 | xargs -0 grep -IniE "$pattern" 2>/dev/null || true)
bad=$(printf '%s\n' "$matches" | grep -vE 'TODO\([^)]+\):' || true)
if [ -n "$bad" ]; then
    printf '%s\n' "$bad"
    exit 1
fi
