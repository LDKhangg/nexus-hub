#!/usr/bin/env bash
# ci-docs-only.sh: print "true" when every file changed by this CI event is
# documentation (*.md, docs/**, LICENSE), else "false". Any doubt (no base,
# tag, manual run, empty diff) prints "false", so the full checks run.
#   pull_request: the PR base ... head
#   push develop: first parent .. HEAD (a merge counts all branch changes)
#   push branch:  merge-base with origin/develop .. HEAD (all branch changes,
#                 so one code commit anywhere on the branch runs everything)
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

event=${GITHUB_EVENT_NAME:-}
ref=${GITHUB_REF:-}
base=""
case "$event" in
  pull_request) base=${PR_BASE_SHA:-} ;;
  push)
    case "$ref" in
      refs/tags/*) ;;
      refs/heads/develop) base=$(git rev-parse -q --verify HEAD^1 || true) ;;
      refs/heads/*)
        git fetch -q --no-tags origin develop 2>/dev/null || true
        mb=$(git merge-base origin/develop HEAD 2>/dev/null || true)
        [ "$mb" != "$(git rev-parse HEAD)" ] && base=$mb ;;
    esac ;;
esac
[ -n "$base" ] || { echo false; exit 0; }

files=$(git diff --name-only "$base" HEAD 2>/dev/null) || { echo false; exit 0; }
[ -n "$files" ] || { echo false; exit 0; }
while IFS= read -r f; do
  case "$f" in
    *.md|docs/*|LICENSE) ;;
    *) echo false; exit 0 ;;
  esac
done <<< "$files"
echo true
