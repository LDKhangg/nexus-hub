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
