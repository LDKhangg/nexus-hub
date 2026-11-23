#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

sed -n '/var specs = map\[string\]spec{/,/^}/s/^[[:space:]]*"\([^"]*\)".*/\1/p' \
  "$root/internal/settings/settings.go" | sort > "$tmp/source"
sed -n '/^## Runtime settings$/,/^## Database migrations$/s/^| `\([^`]*\)` |.*/\1/p' \
  "$root/docs/configuration.md" | sort > "$tmp/docs"

if ! diff -u "$tmp/source" "$tmp/docs"; then
  echo "configuration setting keys do not match internal/settings/settings.go" >&2
  exit 1
fi

if [ "$(sort "$tmp/docs" | uniq -d | wc -l)" -ne 0 ]; then
  echo "docs/configuration.md contains duplicate setting keys" >&2
  exit 1
fi

: > "$tmp/env-source-raw"
for dir in "$root/cmd" "$root/internal"; do
  if [ -d "$dir" ]; then
    find "$dir" -type f -name '*.go' ! -name '*_test.go' -exec \
      grep -hEo '(os\.(Getenv|LookupEnv)|syscall\.Getenv)\(("[^"]+"|`[^`]+`)' {} + >> "$tmp/env-source-raw"
  fi
done
sed 's/^[^(]*(\(["`]\)//; s/["`]$//' "$tmp/env-source-raw" | sort -u > "$tmp/env-source"
sed -n '/^## Process environment$/,/^## Runtime settings$/s/^| `\([^`]*\)` |.*/\1/p' \
  "$root/docs/configuration.md" > "$tmp/env-docs-raw"
sort "$tmp/env-docs-raw" > "$tmp/env-docs"

if ! diff -u "$tmp/env-source" "$tmp/env-docs"; then
  echo "configuration environment variables do not match non-test Go sources" >&2
  exit 1
fi

if [ "$(sort "$tmp/env-docs-raw" | uniq -d | wc -l)" -ne 0 ]; then
  echo "docs/configuration.md contains duplicate environment variables" >&2
  exit 1
fi
