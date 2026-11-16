# Test debt — hotfix/router-schema

Status: CLOSED — TD-RS-1 and TD-RS-2 closed by Review Report phase6b-telegram-topics #A1 on a254393 (commit ea7e25c reviewed: strict authoritative-route parsing, router_error codes only).

- Merged head: see the `Merge hotfix/router-schema` commit on `develop` (code `ea7e25c`, tests `818a50b`).
- Owner decision (2026-11-16): ship the router schema fix right after the live diagnosis, without a Phase Brief test round or a Review.
- Proven: CI suite on the hotfix head (Phase 6a router tests updated to the new contract by the Tester; targeted `-race` runs green); live diagnosis on the VPS with real `claude-haiku-5-5` (shape only): greetings/thanks/general question → `answer` with reply; task/action → `escalate work`; architecture redesign → `escalate deep`.
- Unproven (open):
  - TD-RS-1 (REQ-7, REQ-9 of the Phase 6a plan; FR-043): no Reviewer check of `ea7e25c` — the authoritative-route parsing (answer ignores a valid tier; escalate ignores reply) and the `router_error` code mapping.
  - TD-RS-2 (REQ-4; NFR-030): `router_error` field reviewed for privacy (codes only, no text) by the Reviewer.
- Pay first in the next Phase Brief (Phase 6b).
