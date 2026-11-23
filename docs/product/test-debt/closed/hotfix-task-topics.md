# Test debt — hotfix task topic names

Status: CLOSED 2026-11-18 (Phase 7b Batch A: tests 43be7fa, CI 44042781670 green) — TD-TT-1..7 paid.

- Code head: `92f7920` on `hotfix/task-topic-names`. The owner skipped the Tester and the Reviewer for this hotfix (2026-11-17); only the existing suite ran in CI.
- Unproven (open), plan `docs/plans/2026-11-17-hotfix-task-topics.md`:
  - TD-TT-1 (REQ-1): topic name `<custom id> · <title>`, fallbacks, 128-byte cut with multibyte titles.
  - TD-TT-2 (REQ-2): link message posted once and pinned; claim kept on unclear outcome; pin failure tolerated; ClickUp URL prefix check and fallback URL.
  - TD-TT-3 (REQ-3): `nh telegram topics rename` counts, idempotence, closed topics renamed without a link.
  - TD-TT-4 (REQ-4): `/task` remembered repo, explicit repo wins, unregistered remembered repo falls back to the usage reply.
  - TD-TT-5 (REQ-5): notices for a task keep going to its topic.
  - TD-TT-6: migration 00027 Up/Down.
  - TD-TT-7: topic root-reply — plain text in a forum topic arrives with `reply_to_message` == the topic root and must route as plain text, not as a reply; fixed in `9b761d5`, no regression test.
- Pay first in the next Phase Brief.

## Closed: waived by the owner

2026-11-23: the owner closed stage 1 (GĐ1) and waived all open test debt
("skip luôn"). The open items in this file are not proven; they are closed as
waived, not as passed, and the PCC IDs they cover stay unverified in
`TRACEABILITY.md`.
