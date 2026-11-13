#!/usr/bin/env bash
# check-doc-paths.sh: every Markdown path that the agent guides name in
# backticks must exist, so agents never read a stale or missing file.
# Paths with <placeholders> or globs are skipped. Run from anywhere in the repo.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

guides=(AGENTS.md CLAUDE.md README.md docs/agents/tester.md docs/agents/reviewer.md .opencode/agents/*.md)
missing=0
for f in "${guides[@]}"; do
  [[ -f $f ]] || { echo "missing guide: $f"; missing=1; continue; }
  while IFS=: read -r line path; do
    [[ $path == *"<"* || $path == *"*"* || $path == *'$'* ]] && continue
    # Bare lower-case names like brief.md are examples; bare UPPER-CASE names
    # (AGENTS.md, README.md) are real root files and must exist.
    [[ $path == */* || $path =~ ^[A-Z][A-Z0-9_-]*\.md$ || -e $path ]] || continue
    [[ -e $path ]] || { echo "$f:$line: \`$path\` does not exist"; missing=1; }
  done < <(grep -noE '`[^` ]+\.md`' "$f" | sed -E 's/`//g')
done
# The guides themselves must exist.
for g in AGENTS.md docs/agents/tester.md docs/agents/reviewer.md; do
  [[ -f $g ]] || { echo "guide $g is missing"; missing=1; }
done
exit $missing
