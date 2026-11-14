#!/usr/bin/env bash
# agent-handoff.sh: the Lead hands one brief to an opencode agent and gets its
# report back.
#
#   scripts/agent-handoff.sh <agent> <title> <brief-file> [session-id]
#
# <agent> is implementer, tester or reviewer (.opencode/agents/<agent>.md).
# Without a session id a new session is created; pass the id printed by an
# earlier run to continue it (fix rounds, Test Plan approval, CI run id).
#
# The run goes through opencode's shared background server, so the owner can
# open the same session in their opencode window and watch it live.
#
# Output, under .superpowers/handoff/:
#   <stamp>-<agent>.ndjson  raw event stream
#   <stamp>-<agent>.md      the agent's final text (its report)
# The script prints the session id and the report path.
#
# Environment: HANDOFF_TIMEOUT (seconds, default 5400), HANDOFF_MODEL (overrides
# the agent's model, for example openai/gpt-5.6-sol-fast).
set -euo pipefail

if [[ $# -lt 3 || $# -gt 4 ]]; then
  echo "usage: $0 <implementer|tester|reviewer> <title> <brief-file> [session-id]" >&2
  exit 2
fi
agent=$1 title=$2 brief=$3 session=${4:-}

case $agent in
  implementer|tester|reviewer) ;;
  *) echo "unknown agent: $agent" >&2; exit 2 ;;
esac
[[ -f $brief ]] || { echo "brief not found: $brief" >&2; exit 2; }

root=$(command git rev-parse --show-toplevel)
cd "$root"
outdir="$root/.superpowers/handoff"
mkdir -p "$outdir"
stamp=$(date +%Y%m%d-%H%M%S)-$$
events="$outdir/$stamp-$agent.ndjson"
report="$outdir/$stamp-$agent.md"

args=(run --agent "$agent" --format json --title "$title" --file "$brief")
[[ -n $session ]] && args+=(--session "$session")
[[ -n ${HANDOFF_MODEL:-} ]] && args+=(--model "$HANDOFF_MODEL")

msg="Read the attached brief and do exactly what it asks, following your agent instructions. End with the report your instructions define."

status=0
timeout --kill-after=30 "${HANDOFF_TIMEOUT:-5400}" \
  opencode "${args[@]}" "$msg" </dev/null >"$events" 2>"$events.err" || status=$?

sid=$(jq -rn 'first(inputs | select(.sessionID != null) | .sessionID) // empty' "$events" 2>/dev/null || true)
# The report is the text of the last step that produced text.
jq -rs '
  [ .[] | select(.type == "text") | .part.text // empty ] as $t
  | if ($t | length) == 0 then "" else $t[-1] end' "$events" >"$report" 2>/dev/null || true

# Token use of this run and the size of the session context at its end.
usage=$(jq -rs '
  [ .[] | select(.type == "step_finish") | .part.tokens ] as $t
  | if ($t | length) == 0 then "steps=0"
    else "steps=\($t | length) input=\([$t[].input] | add) cache_read=\([$t[].cache.read // 0] | add) output=\([$t[].output] | add) context_end=\(($t[-1].input // 0) + ($t[-1].cache.read // 0))"
    end' "$events" 2>/dev/null || echo "steps=?")
ctx=$(sed -n 's/.*context_end=\([0-9]*\).*/\1/p' <<<"$usage")

echo "agent:   $agent"
echo "session: ${sid:-unknown}"
echo "exit:    $status"
echo "tokens:  $usage"
if [[ -n $ctx && $ctx -gt 100000 ]]; then
  echo "warning: session context is ${ctx} tokens; start a new session with a carry-over note next time"
fi
if grep -q '"provider.quota"' "$events" 2>/dev/null; then
  echo "warning: provider quota reached; see the error event in $events" >&2
fi
echo "report:  $report"
echo "events:  $events"
# The KEY block is what the Lead reads first.
if grep -q '^KEY' "$report" 2>/dev/null; then
  echo "---"
  sed -n '/^KEY/,/^- Next:/p' "$report"
fi
if [[ $status -ne 0 ]]; then
  echo "stderr (last lines):" >&2
  tail -n 5 "$events.err" >&2 || true
fi
exit "$status"
