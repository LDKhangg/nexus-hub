#!/usr/bin/env bash
# check-doc-paths.sh: every Markdown path that the agent guides name in
# backticks must exist, so agents never read a stale or missing file.
# Paths with <placeholders> or globs are skipped. Run from anywhere in the repo.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

guides=(AGENTS.md CLAUDE.md README.md TESTER.md REVIEWER.md .opencode/agents/*.md)
missing=0
for f in "${guides[@]}"; do
  [[ -f $f ]] || { echo "missing guide: $f"; missing=1; continue; }
  while IFS=: read -r line path; do
    [[ $path == *"<"* || $path == *"*"* || $path == *'$'* ]] && continue
    [[ $path == */* || -e $path ]] || continue   # bare names like brief.md are examples
    [[ -e $path ]] || { echo "$f:$line: \`$path\` does not exist"; missing=1; }
  done < <(grep -noE '`[^` ]+\.md`' "$f" | sed -E 's/`//g')
done
# Bare guide names must exist at the root.
for g in AGENTS.md TESTER.md REVIEWER.md; do
  [[ -f $g ]] || { echo "root guide $g is missing"; missing=1; }
done
exit $missing
