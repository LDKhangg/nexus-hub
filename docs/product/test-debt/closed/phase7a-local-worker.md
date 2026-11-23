# Test debt — Phase 7a local worker

Status: PARTLY CLOSED 2026-11-18 — TD-7A-2..4 paid in Phase 7b Batch A (tests 43be7fa, CI 44042781670); TD-7A-1 (live laptop check) stays open with the owner.

- Code head: `c85e9ca` on `feature/phase7a` (CI 44009727911 green, Review APPROVE).
- Unproven (open):
  - TD-7A-1: live check on the owner's laptop — enroll, `/task` from a task topic opens a draft PR, offline → waiting message, lid closed mid-job → interrupted/resume (plan §6). Runs after deploy.
  - TD-7A-2: no regression test for D-B-5 (`/task please do X` outside a topic replies usage; ef7df99).
  - TD-7A-3: no local-task-specific test that topic text during a running job becomes an instruction (heartbeat/ack part is covered by TestB3Store_FollowupsOverHeartbeat).
  - TD-7A-4: telemetry threshold texts with real worker telemetry, and Sender 401 during flush, are not covered.
- Pay first in the Phase 7b Phase Brief.

## Closed: waived by the owner

2026-11-23: the owner closed stage 1 (GĐ1) and waived all open test debt
("skip luôn"). The open items in this file are not proven; they are closed as
waived, not as passed, and the PCC IDs they cover stay unverified in
`TRACEABILITY.md`.
