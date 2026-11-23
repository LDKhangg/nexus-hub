# PCC-001 traceability matrix

Status of every PCC-001 requirement. Statuses follow PCC §10.1.2: `PENDING`, `IN_PROGRESS`, `VERIFIED`, `BLOCKED`. Only the Lead edits this file, and only from evidence: `VERIFIED` needs a Test Report `PASS` and a Review Report `APPROVE` on the same merged commit, cited in the Evidence column. "Phase" is the repo phase in `docs/product/ROADMAP.md`.

Snapshot: 2026-11-12. Phase 1 merged; Phase 2 under test and review. Nothing is `VERIFIED` against the PCC yet: Phase 1/2 work covers parts of the IDs marked `IN_PROGRESS`.

Update 2026-11-23 (Phase 12): reliability, observability, data ops and security hardening implemented; per-batch Test PASS + Review APPROVE, final CI 45111228514 green on feature/phase12 code 7c25c37; touched IDs IN_PROGRESS until merge. Gate B and Gate C evidence: reports/2026-11-23-phase12-report.md and reports/2026-11-23-phase12-gate-c.md. NFR-006 restore drill = test debt D12-1.

Update 2026-11-19 (Phase 9): policy engine and approvals implemented, Test PASS (CI 44363609327) + Review APPROVE on feature/phase9 code 9bb65d5; touched IDs noted per row, IN_PROGRESS until merge. FR-117 (UI) stays Phase 11.

Update 2026-11-18 (Phase 10): task topics and ClickUp outbound implemented, Test PASS (CI 44332870926) + Review APPROVE on feature/phase10 code a507263; touched IDs noted per row, IN_PROGRESS until merge. FR-134, FR-136, FR-137 moved to Phase 10b (unchanged).

Update 2026-11-18: owner decision D7 re-orders the roadmap after 7b: Phase 10 next (ends MVP 1), then 9, 12, 13; Phase 8 (multi-slot, scheduler) deferred. IDs with phase `8` only (FR-101, FR-102, FR-104, FR-105, NFR-009, E2E-002) stay `PENDING` while deferred. No status changed.

Update 2026-11-16: roadmap Phase 7 split into 7a (local worker bridge, plan approved) and 7b (AI orchestrator, the former Phase 7 plan). Phase column: orchestrator IDs `7` → `7b`; IDs covered by the 7a plan gained `7a`. No status changed.

Update 2026-11-15: Phase 5 (durable workflow engine + open Telegram assistant) merged into develop 65be4ac (CI 43631958237 green) after Test PASS and Review APPROVE on 2ac779b; its scoped IDs are VERIFIED. E2E-005 live part stays open (Phase 12/13).

Update 2026-11-13: Phases 2, 3, 3d, 3c, 3e merged to main (6c9a0a6); CI 43394889383 green (664 tests). Open test debt: TD-6, TD3-2, TD3-3, TD3d-2, TD3d-3 (independent reviews of Phases 2/3/3d and live ClickUp payload facts). Per-ID statuses are updated per phase when the owner asks.

| ID | Requirement | Phase | Status | Evidence / note |
|---|---|---|---|---|
| FR-001 | Có persistent internal user ID dạng UUID. | 4 | PENDING |  |
| FR-002 | Một user liên kết được nhiều external identities. | 4 | PENDING |  |
| FR-003 | Hỗ trợ Telegram identity ở MVP. | 4 | PENDING |  |
| FR-004 | Có flow xác minh quyền sở hữu external identity trước khi liên kết. | 4 | PENDING |  |
| FR-005 | Mapping external identity sang internal user. | 4 | PENDING |  |
| FR-006 | Mỗi user có user settings và rule sets riêng. | 4 | PENDING |  |
| FR-007 | Có thể revoke một external identity. | 4 | PENDING |  |
| FR-008 | Không chấp nhận message từ identity chưa được cho phép để thực thi tác vụ. | 4 | PENDING |  |
| FR-009 | Các sensitive actions yêu cầu quyền hoặc approval phù hợp. | 4, 9 | IN_PROGRESS | Phase 9: dangerous tools need a bound owner approval (v1: github_pr_merge approval_required) (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-010 | Platform Adapter Registry. | 4 | PENDING |  |
| FR-011 | Capability discovery. | 4 | PENDING |  |
| FR-012 | Inbound event normalization. | 4, 6b | IN_PROGRESS | Phase 6b: Test PASS (CI 43823427615) + Review APPROVE on feature/phase6b 6b514c8: Telegram inbound carries the forum thread id; full gateway Phase 4b |
| FR-013 | Outbound action execution. | 10 | IN_PROGRESS | Phase 10: ClickUp comment post (threaded reply, structured mentions, status) only through draft + owner confirm; orchestrator `clickup.write` creates a draft (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-014 | Connection lifecycle management. | 10 | PENDING |  |
| FR-015 | Credentials được mã hóa khi lưu. | 2 | IN_PROGRESS | Phase 2: AES-256-GCM credentials table (awaiting Test/Review PASS) |
| FR-016 | Health check cho integration. | 10 | PENDING |  |
| FR-017 | Hỗ trợ webhook signature verification theo từng platform. | 4, 10 | IN_PROGRESS | Phase 1: ClickUp HMAC verified; Telegram (P4) and GitHub (P10) missing |
| FR-018 | Idempotency cho inbound events. | 4 | IN_PROGRESS | Phase 1: idempotent ClickUp ingest; generic inbox in P4 |
| FR-019 | Rate-limit và retry handling. | 2, 10 | IN_PROGRESS | Phase 2: Telegram/GitHub retry and rate-limit handling; outbound generic in P10 |
| FR-020 | Có integration diagnostics trong Admin UI. | 11 | PENDING |  |
| FR-021 | Event schema có version. | 4 | PENDING |  |
| FR-022 | Validate event trước khi xử lý. | 4 | PENDING |  |
| FR-023 | Persist raw event metadata cần thiết. | 4 | PENDING |  |
| FR-024 | Có normalized event. | 4 | PENDING |  |
| FR-025 | Có event deduplication. | 4 | PENDING |  |
| FR-026 | Correlation ID xuyên suốt execution. | 4 | PENDING |  |
| FR-027 | Unknown event types được xử lý an toàn. | 4 | PENDING |  |
| FR-028 | Có event replay phù hợp từ dữ liệu đã lưu. | 4 | PENDING |  |
| FR-029 | Event processing không phụ thuộc vào tốc độ phản hồi của LLM. | 2, 4 | IN_PROGRESS | Phase 1/2: webhook stores then processes asynchronously |
| FR-030 | Persistent conversations. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988 |
| FR-031 | Conversation-to-user binding. | 6 | IN_PROGRESS | Phase 6: bound to workspace + owner chat (Q3a, nullable user_id seam); users table later (Phase 4b) |
| FR-032 | Conversation-to-workflow binding. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988 |
| FR-033 | Một conversation hỗ trợ nhiều workflows. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988; Phase 6b: Test PASS (CI 43823427615) + Review APPROVE on feature/phase6b 6b514c8: per-topic conversations and topic-aware follow-up resolution |
| FR-034 | Một workflow liên kết được với nhiều external resources. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988; Phase 6b: Test PASS (CI 43823427615) + Review APPROVE on feature/phase6b 6b514c8: per-topic conversations and topic-aware follow-up resolution |
| FR-035 | Context resolution cho follow-up messages. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988; Phase 6b: Test PASS (CI 43823427615) + Review APPROVE on feature/phase6b 6b514c8: per-topic conversations and topic-aware follow-up resolution |
| FR-036 | Có workflow reference ID để định danh tường minh. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988 |
| FR-037 | Nếu không rõ workflow đích, hệ thống phải hỏi lại. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988 |
| FR-038 | Hỗ trợ bổ sung instructions khi workflow đang chạy. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988 |
| FR-039 | Version hóa instruction updates. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988 |
| FR-040 | Hỗ trợ cancel, resume, status và list từ conversation. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988 |
| FR-041 | Không trộn context của hai workflow độc lập. | 6 | VERIFIED | Phase 6: Test PASS (CI 43670776642, 1258 tests, 0 skip) + Review APPROVE on feature/phase6 52af5e0; merged d7e4988; Phase 6b: Test PASS (CI 43823427615) + Review APPROVE on feature/phase6b 6b514c8: per-topic conversations and topic-aware follow-up resolution |
| FR-042 | Có cơ chế giới hạn và tóm tắt context khi cần. | 2, 6, 6a | IN_PROGRESS | Phase 2: rolling summary + packet budget; Phase 6: persisted conversation summary ≤2,000 bytes, 8 KiB instruction block (52af5e0); Phase 6a: assistant history = summary + last N, cached (8bd07d9) |
| FR-043 | Intent classification. | 2, 7b, 6a | IN_PROGRESS | Phase 2: Tier-1 triage categories; Phase 6a: assistant router answer/work/deep (8bd07d9); Phase 7b: Tier-1 triage through Classify (classify@2) (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-044 | Task type classification. | 7b | DONE | Phase 7b: Classify task type (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-045 | Task priority proposal. | 2, 7b | IN_PROGRESS | Phase 2: urgency in triage; Phase 7b: Classify priority (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-046 | Complexity estimation. | 7b | DONE | Phase 7b: Classify complexity (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-047 | Capability requirements. | 7b | DONE | Phase 7b: plan task capabilities, Go-checked (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-048 | Execution plan generation. | 7b | DONE | Phase 7b: Plan with Go checks, /plan + start_task (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-049 | Worker selection recommendation. | 7b, 10 | DONE | Phase 7b: least-privilege executor matching, repo.write → 7a worker (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending); Phase 10: `behavior-actions` executor wired (clickup.write → draft, telegram.notify) (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-050 | Task decomposition. | 7b | DONE | Phase 7b: decomposition with dependencies (DAG) (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-051 | Result evaluation. | 7b | DONE | Phase 7b: Evaluate with evidence refs (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-052 | Next-action recommendation. | 7b | DONE | Phase 7b: 7 decisions + next action (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-053 | Clarification request generation. | 7b | DONE | Phase 7b: REQUEST_CLARIFICATION bound to WF-n (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-054 | Plan revision. | 7b | DONE | Phase 7b: REPLAN v+1, finished tasks carried (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-055 | Structured outputs và schema validation. | 2, 7b | IN_PROGRESS | Phase 2: triage JSON validated; Phase 7b: versioned JSON schemas + bounded repair (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-056 | Configurable reasoning/iteration budget. | 2, 7b, 6a, 10 | IN_PROGRESS | Phase 2: token budget, step limit; Phase 6a: fast/work/deep model settings, history turns (8bd07d9); Phase 7b: per-execution budgets, O4a gate (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending); Phase 10: wall-time budget enforced while waiting for a worker (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-057 | Worker registry. | 7a, 8 | IN_PROGRESS | Phase 7a: device enrollment, hashed tokens, revoke (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-058 | Worker capability profiles. | 7a, 8 | IN_PROGRESS | Phase 7a: device capability profiles (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-059 | Worker availability tracking. | 7a, 8 | IN_PROGRESS | Phase 7a: presence, offline after 90 s, telemetry card (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-060 | Worker task assignment. | 7a, 8 | IN_PROGRESS | Phase 7a: lease protocol, /task → local-task worker.run (c85e9ca, CI 44009727911; live laptop check pending); Phase 7b: DELEGATE → 7a worker job (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending) |
| FR-061 | Isolated working directories. | 7a, 8 | IN_PROGRESS | Phase 7a: per-job git worktree on the laptop (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-062 | Tool permission enforcement. | 2, 8 | IN_PROGRESS | Phase 2: tiered tool policy executor |
| FR-063 | Execution timeout. | 2, 7a, 8, 10 | IN_PROGRESS | Phase 2: tool timeouts; Phase 7a: 60 min wall, turn limit, test timeout, sleep inhibitor (c85e9ca, CI 44009727911; live laptop check pending); Phase 10: orchestrated worker wait ends at the wall-time budget (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-064 | Cancellation support. | 7a, 8, 10 | IN_PROGRESS | Phase 7a: /cancel and Cancel button stop the executor (c85e9ca, CI 44009727911; live laptop check pending); Phase 10: Cancel on orchestrated worker jobs (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-065 | Captured tool results. | 2, 8 | IN_PROGRESS | Phase 2: tool_runs audit with summaries |
| FR-066 | Task progress reporting. | 7a, 8, 10 | IN_PROGRESS | Phase 7a: one in-place progress message per job (c85e9ca, CI 44009727911; live laptop check pending); Phase 10: orchestrated worker jobs show the 7a progress message (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-067 | Worker result contract. | 7a, 8 | IN_PROGRESS | Phase 7a: idempotent result contract (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-068 | Worker failure reporting. | 7a, 8, 10 | IN_PROGRESS | Phase 7a: fixed failure reason codes, Retry/Cancel buttons (c85e9ca, CI 44009727911; live laptop check pending); Phase 10: orchestrated worker jobs show Retry/Cancel; the execution sees the outcome (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-069 | Worker execution iteration limit. | 2, 8 | IN_PROGRESS | Phase 2: Tier-2 step limit |
| FR-070 | Artifact references. | 7a, 8 | IN_PROGRESS | Phase 7a: result carries branch, PR URL, artifact refs (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-071 | Workspace cleanup và retention. | 7a, 8 | IN_PROGRESS | Phase 7a: worktree retention and nh worker clean (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-072 | Ngăn concurrent write conflicts đối với cùng repository/branch. | 7a, 8 | IN_PROGRESS | Phase 7a: one write lock per repo+branch (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-073 | Create workflow definition. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-074 | Update draft workflow. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-075 | Validate workflow schema. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-076 | Publish workflow version. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-077 | Enable/disable workflow. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-078 | Define triggers. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-079 | Define orchestration settings. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-080 | Define worker constraints. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-081 | Define notification rules. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-082 | Define approval rules. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-083 | Define retries/timeouts. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-084 | Persist immutable published versions. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-085 | Execution luôn tham chiếu một workflow version cụ thể. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-086 | Có cơ chế xuất/nhập config dạng JSON/YAML. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-087 | State machine với valid transitions. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-088 | Persistent execution records. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-089 | Step-level checkpoints. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-090 | Event history. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-091 | Retry policy. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-092 | Cancellation. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-093 | Resume/recovery. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-094 | Execution timeout. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-095 | Dead-letter hoặc failed-job inspection. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-096 | Dependency-aware task progression. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-097 | Không giữ worker khi đang chờ user. | 7a, 8, 10 | IN_PROGRESS | Phase 7a: job waits WAITING_FOR_WORKER, no VPS fallback (c85e9ca, CI 44009727911; live laptop check pending); Phase 10: orchestrated worker waits use WAITING_FOR_WORKER (00033) and wake on the job result or wall time (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-098 | Optimistic locking/leases để tránh nhiều scheduler cùng claim một job. | 2, 5 | IN_PROGRESS | Phase 2: agent job leases + thread lock; Phase 5: generic queue leases + fenced claims, merged develop 65be4ac |
| FR-099 | Reconciliation sau process restart. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-100 | Persistent job queue. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-101 | Configurable per-user workflow concurrency. | 8 | PENDING |  |
| FR-102 | Configurable worker pool size. | 8 | PENDING |  |
| FR-103 | Worker capability matching. | 7a, 8 | IN_PROGRESS | Phase 7a: capability and telemetry-aware matching (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-104 | Priority scheduling. | 8 | PENDING |  |
| FR-105 | Resource conflict handling. | 8 | PENDING |  |
| FR-106 | Job leases và lease expiration. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-107 | Worker heartbeat hoặc tương đương. | 7a, 8 | IN_PROGRESS | Phase 7a: heartbeat, lease expiry, reissue after lost poll (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-108 | Queue visibility. | 5 | VERIFIED | Phase 5: Test PASS (CI 43630791438, 1120 tests) + Review APPROVE on 2ac779b; merged develop 65be4ac (CI 43631958237 green) |
| FR-109 | Backpressure khi quá tải. | 7a, 8 | IN_PROGRESS | Phase 7a: telemetry thresholds hold jobs (single device) (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-110 | Worker allocation được audit. | 7a, 8 | IN_PROGRESS | Phase 7a: worker_events audit (c85e9ca, CI 44009727911; live laptop check pending) |
| FR-111 | Rule schema có version. | 9 | IN_PROGRESS | Phase 9: rule sets `policy/v1` with versions, history and rollback (00035) (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-112 | Rule validation. | 9 | IN_PROGRESS | Phase 9: strict Parse/Validate (closed keys and effects, known actions only); CLI validate/apply (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-113 | Policy evaluation trước các privileged actions. | 2, 9 | IN_PROGRESS | Phase 2: policy before tool calls (tools only); Phase 9: one policy engine decides every non-read tool call and the orchestrator merge (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-114 | Deny-by-default khi không đủ quyền. | 2, 9 | IN_PROGRESS | Phase 2: nothing allowed by default (tools only); Phase 9: unknown action, missing/invalid/edited set, store or audit failure → deny or safe set (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-115 | Approval-gated actions. | 9 | IN_PROGRESS | Phase 9: approval_required effect; merge waits for the owner (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-116 | Policy decision audit. | 2, 9 | IN_PROGRESS | Phase 2: tool_runs audit incl. denied; Phase 9: policy_decisions row per decision (ids, names, hashes only) (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-117 | Rule management qua UI. | 9, 11 | PENDING |  |
| FR-118 | Không cho AI sửa system-level policy tùy tiện. | 9 | IN_PROGRESS | Phase 9: only `nh policy` (CLI) and `nh init` write rule sets; no assistant tool, action or capability (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-119 | Chống prompt injection từ webhook, issues, comments và repository content. | 2, 9 | IN_PROGRESS | Phase 2: sanitizer, policy is the boundary |
| FR-120 | Phân biệt trusted instructions với untrusted data. | 2, 9 | IN_PROGRESS | Phase 2: untrusted content marked in prompts |
| FR-121 | Generate approval request. | 9 | IN_PROGRESS | Phase 9: policy_gate approval created for an orchestrated merge (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-122 | Bind approval với user, execution, action và resource cụ thể. | 9 | IN_PROGRESS | Phase 9: gate bound to workspace, owner member, execution, action, resource key, input hash (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-123 | Deliver approval notification. | 9 | IN_PROGRESS | Phase 9: 🔐 Cần duyệt message with Duyệt/Từ chối in the execution topic (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-124 | Approve. | 9 | IN_PROGRESS | Phase 9: Duyệt resumes the task and merges once (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-125 | Reject. | 9 | IN_PROGRESS | Phase 9: Từ chối blocks the task (approval_rejected), nothing merged (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-126 | Approval expiration. | 9 | IN_PROGRESS | Phase 9: gate expiry = min(rule TTL, execution deadline); sweep closes gates of ended executions (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-127 | Prevent duplicate approvals. | 9 | IN_PROGRESS | Phase 9: one open gate per action/resource/input; single-use grant (consumed_at) (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-128 | Resume workflow sau approval. | 9 | IN_PROGRESS | Phase 9: execution waits WAITING_FOR_APPROVAL and wakes on the decision (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-129 | Ghi lại actor và thời gian quyết định. | 9 | IN_PROGRESS | Phase 9: decided_by (Telegram id) + decided_user_id + time (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-130 | Revalidate policy/action context trước khi thực thi sau approval. | 9 | IN_PROGRESS | Phase 9: policy decided again right before the side effect; head SHA pinned (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| FR-131 | Destination resolution. | 6b, 10 | IN_PROGRESS | Phase 6b: Test PASS (CI 43823427615) + Review APPROVE on feature/phase6b 6b514c8: Telegram task/workflow/fixed topics with General fallback; other platforms Phase 10; Phase 10: ClickUp task events and general behavior reports go to the task's forum topic; closed topic reopened; deleted topic → General once with 📌 button (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-132 | Original thread replies. | 6b, 10 | IN_PROGRESS | Phase 6b: Test PASS (CI 43823427615) + Review APPROVE on feature/phase6b 6b514c8: Telegram replies stay in the inbound forum topic (is_topic_message); GitHub/ClickUp Phase 10; Phase 10: Telegram message ↔ ClickUp comment links; a reply (or `/comment`) on a linked comment drafts a threaded ClickUp reply (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-133 | Cross-platform notifications. | 10 | IN_PROGRESS | Phase 10: ClickUp activity in the Telegram task topic; `/task`/`/plan` result posted back to ClickUp via 📤 Đăng lên ClickUp (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-134 | On-started notification policy. | 10 | PENDING |  |
| FR-135 | On-completed notification policy. | 10 | IN_PROGRESS | Phase 10: completed/failed `/task` and `/plan` reports carry 📤 (draft with summary, PR link, outcome line) (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-136 | On-blocked notification policy. | 10 | PENDING |  |
| FR-137 | On-failed notification policy. | 10 | PENDING |  |
| FR-138 | Notification deduplication. | 10 | IN_PROGRESS | Phase 10: no duplicate topic message on webhook redelivery; own-bot comments never shown; one draft per press (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-139 | Transactional outbox. | 1, 10 | IN_PROGRESS | Phase 1: notification_outbox; Phase 2: outbound_actions; Phase 10: ClickUp drafts table (00032/00034) with idempotent post key = draft id (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-140 | Delivery retry. | 1, 10 | IN_PROGRESS | Phase 1: notice retry with backoff; Phase 10: Thử lại on a failed post (same idempotency key); posting-state sweep replays stuck posts (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-141 | Delivery status tracking. | 1, 10 | IN_PROGRESS | Phase 1: delivery status in outbox; Phase 10: draft states pending/posting/posted/cancelled/expired/failed/failed_final shown in the preview (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| FR-142 | Rate limiting và notification aggregation. | 10 | PENDING |  |
| FR-143 | Có fallback destination nếu platform chính không khả dụng và user đã cấu hình. | 10 | PENDING |  |
| UI-001 | Hiển thị active workflows. | 11 | PENDING |  |
| UI-002 | Hiển thị worker status. | 11 | PENDING |  |
| UI-003 | Hiển thị failed và blocked executions. | 11 | PENDING |  |
| UI-004 | Hiển thị pending approvals. | 11 | PENDING |  |
| UI-005 | Add/remove integration. | 11 | PENDING |  |
| UI-006 | Configure credentials qua flow an toàn. | 11 | PENDING |  |
| UI-007 | Test connection. | 11 | PENDING |  |
| UI-008 | View connection health. | 11 | PENDING |  |
| UI-009 | Create/edit workflow. | 11 | PENDING |  |
| UI-010 | Form-based workflow config. | 11 | PENDING |  |
| UI-011 | JSON/YAML preview. | 11 | PENDING |  |
| UI-012 | Validate config. | 11 | PENDING |  |
| UI-013 | Publish version. | 11 | PENDING |  |
| UI-014 | Enable/disable workflow. | 11 | PENDING |  |
| UI-015 | Execution list. | 11 | PENDING |  |
| UI-016 | Execution detail/timeline. | 11 | PENDING |  |
| UI-017 | Step outputs. | 11 | PENDING |  |
| UI-018 | Errors và retry history. | 11 | PENDING |  |
| UI-019 | Cancel/retry khi được phép. | 11 | PENDING |  |
| UI-020 | Links đến external resources/artifacts. | 11 | PENDING |  |
| UI-021 | Edit user rules. | 11 | PENDING |  |
| UI-022 | Configure approvals. | 11 | PENDING |  |
| UI-023 | Configure notifications. | 11 | PENDING |  |
| UI-024 | Configure models/workers. | 11 | PENDING |  |
| UI-025 | Configure concurrency/timeout limits. | 11 | PENDING |  |
| UI-026 | Admin authentication. | 11 | PENDING |  |
| UI-027 | Session management. | 11 | PENDING |  |
| UI-028 | Protected write operations. | 11 | PENDING |  |
| UI-029 | Không hiển thị credentials nguyên bản. | 11 | PENDING |  |
| UI-030 | Responsive mobile layout cho status và approvals. | 11 | PENDING |  |
| NFR-001 | Không mất acknowledged inbound events khi process restart. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12a store-then-ack + dedup |
| NFR-002 | Có crash recovery. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12a reconciler/sweeper resume |
| NFR-003 | Có durable queues. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12a durable queues, leases |
| NFR-004 | Có idempotency strategy. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12a dedup + idempotency keys |
| NFR-005 | Có graceful shutdown. | 2, 12 | IN_PROGRESS | Phase 1/2: runServices graceful shutdown; Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12a shutdown_timeout drain |
| NFR-006 | Có database backup và restore đã được thử nghiệm. | 12 | IN_PROGRESS | Phase 12: restore drill not done — test debt D12-1 (test-debt/phase12-backup-restore.md); manual pg_dump steps in runbook |
| NFR-007 | Với local database khỏe mạnh, webhook handler có p95 dưới 500ms, không tính external provider processing. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): load test p95 59.2 ms (loadtest-2026-11-20.json) |
| NFR-008 | Sau khi webhook được xác thực và ghi nhận, việc tạo execution không cần chờ LLM hoàn tất. | 1, 4 | IN_PROGRESS | Phase 1/2: ingest does not wait for LLM |
| NFR-009 | Ít nhất hai worker tasks độc lập có thể thực thi đồng thời. | 8 | PENDING |  |
| NFR-010 | Tối thiểu 100 workflow executions được lưu/truy vấn ổn định trong bài test MVP. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): load test 120 executions |
| NFR-011 | UI không bị block bởi workflow dài. | 11 | PENDING |  |
| NFR-012 | Có configurable resource quotas và backpressure. | 8, 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12a queue/workspace caps (multi-slot stays Phase 8) |
| NFR-013 | HTTPS cho public endpoints. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12d HTTPS via Cloudflare tunnel documented, Gate C report |
| NFR-014 | Authentication cho Admin UI. | 11 | PENDING |  |
| NFR-015 | Webhook authenticity verification. | 1, 4 | IN_PROGRESS | Phase 1: ClickUp signature verification |
| NFR-016 | Secrets encryption at rest và secret redaction. | 2 | IN_PROGRESS | Phase 2: encryption at rest + redactor |
| NFR-017 | Least privilege cho integrations. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12d git bwrap sandbox + github.com-only proxy; live check pending |
| NFR-018 | Không expose management endpoints không xác thực. | 11 | PENDING |  |
| NFR-019 | Sandboxed/restricted worker execution. | 2, 7a, 8, 12 | IN_PROGRESS | Phase 2: hardened git-only runner; Phase 7a: file-tools-only claude, no creds for executor/tests (c85e9ca, CI 44009727911; live laptop check pending) |
| NFR-020 | Chống path traversal và command injection tại tool boundaries. | 2, 12 | IN_PROGRESS | Phase 2: path/argument checks in tools |
| NFR-021 | Audit privileged actions. | 2, 12 | IN_PROGRESS | Phase 2: tool_runs audit |
| NFR-022 | Prompt-injection defenses và trust-boundary checks. | 2, 12 | IN_PROGRESS | Phase 2: sanitizer + policy boundary |
| NFR-023 | Không cho workflow hoặc message nội dung không tin cậy chỉnh sửa system policy. | 9 | IN_PROGRESS | Phase 9: no workflow, model or message path can change policy (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| NFR-024 | Có data retention và deletion controls. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12c retention sweeper + nh retention |
| NFR-025 | Structured logs. | 3 | IN_PROGRESS | Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): JSON slog for server and nh, std log bridged, redacting handler (REQ-1, REQ-3) |
| NFR-026 | Correlation IDs. | 3 | IN_PROGRESS | Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): UUIDv7 correlation id per webhook/Telegram/worker call, stored on inbound rows, agent jobs and executions; in every step log (REQ-2) |
| NFR-027 | Execution timelines. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12b nh execution timeline |
| NFR-028 | Worker status và queue depth. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12b /metrics loopback + nh status |
| NFR-029 | Error metrics. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12b error metrics |
| NFR-030 | LLM usage/cost tracking khi provider cung cấp dữ liệu. | 2, 12, 6a | IN_PROGRESS | Phase 2: token usage per job; Phase 6a: per-turn assistant usage log incl. router_* (8bd07d9); Phase 7a: worker usage and Claude limits in telemetry (c85e9ca, CI 44009727911; live laptop check pending); Phase 12: 12b metrics |
| NFR-031 | Health/readiness endpoints. | 3 | IN_PROGRESS | Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): public /healthz; /readyz on the loopback metrics listener (REQ-4) |
| NFR-032 | Có cảnh báo hoặc notification cho stuck/failed workflows. | 12 | IN_PROGRESS | Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): 12b Telegram stuck/failed alerts |
| NFR-033 | Có README. | 3 | IN_PROGRESS | Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): README Run it locally, docs/configuration.md, nh seed demo (REQ-6..8) |
| NFR-034 | Có architecture documentation. | every, 10, 9 | IN_PROGRESS | docs/architecture.md predates PCC; ROADMAP.md added; Phase 6 runbook section; Phase 7b: docs/architecture.md §7 AIRuntime boundary (feature/phase7b e4c979b, CI 44225394222, Review APPROVE; fake provider; live /plan check pending); Phase 10: runbook "Task topics and ClickUp replies (Phase 10)" incl. `/comment`; architecture note on destination resolution and message links (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge); Phase 9: runbook "Policy and approvals (Phase 9)"; architecture §9 (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| NFR-035 | Có database migrations. | 3 | IN_PROGRESS | goose migrations 00001–00003; Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): goose migrations through 00043; CI goose validate and generate drift check (REQ-9) |
| NFR-036 | Có OpenAPI hoặc tài liệu API tương đương. | 11 | PENDING |  |
| NFR-037 | Có automated tests. | 3 | IN_PROGRESS | Go tests exist; CI missing (P3); Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): full suite with -race in CI on every push (3290 tests at a58b320) |
| NFR-038 | Có CI pipeline. | 3 | IN_PROGRESS | Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): CI: gofmt, vet, build, secret scan, generate drift, config doc check, -race suite (REQ-9) |
| NFR-039 | Có deployment instructions. | 13 | PENDING |  |
| NFR-040 | Có recovery/runbook documentation. | 12 | IN_PROGRESS | runbooks/clickup-telegram-local.md (local only); Phase 12 (feature/phase12, code 7c25c37, CI 45111228514; report reports/2026-11-23-phase12-report.md): runbook Recovery (Phase 12); restore part = test debt D12-2 |
| DEP-001 | Reproducible deployment bằng Docker Compose và/hoặc systemd cho worker runtime. | 13 | PENDING |  |
| DEP-002 | Environment configuration document. | 3, 13 | IN_PROGRESS | Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): docs/configuration.md with CI drift check both ways (REQ-6) |
| DEP-003 | Secrets không commit vào repository. | 3, 13 | IN_PROGRESS | .env.example has no values; secrets in DB; Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): .gitignore env/key/dump; CI secret scan (Phase 12); seed uses fake credentials (REQ-10) |
| DEP-004 | Database migrations tự động hoặc có bước deploy rõ ràng. | 3, 13 | IN_PROGRESS | Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): documented deploy step: deploy.sh runs goose before the switch (Q2); local goose up in README |
| DEP-005 | Startup health checks. | 3, 13 | IN_PROGRESS | Phase F (feature/phaseF, head a58b320, CI 45144063548; report reports/2026-11-23-phaseF-report.md): startup readiness summary, DB failure fatal, /readyz checks (REQ-4, REQ-5) |
| DEP-006 | Graceful shutdown. | 13 | PENDING |  |
| DEP-007 | Persistent volumes. | 13 | PENDING |  |
| DEP-008 | Backup scheduling. | 13 | PENDING |  |
| DEP-009 | Logs rotation/retention. | 13 | PENDING |  |
| DEP-010 | Cấu hình secure remote access. | 13 | PENDING |  |
| DEP-011 | Có hướng dẫn nâng version và rollback deployment. | 13 | PENDING |  |
| DEP-012 | Có fresh-install test trên môi trường sạch. | 13 | PENDING |  |
| E2E-001 | Mobile Prompt to Pull Request | 10, 13 | PENDING |  |
| E2E-002 | Multiple Concurrent Workflows | 8, 13 | PENDING |  |
| E2E-003 | Update Running Workflow | 6, 8, 13 | IN_PROGRESS | Phase 6: fake-provider journey (reply → v1 → next model/investigate step → /status) on 52af5e0; worker-delivery part Phase 8, live part Phase 13; Phase 6b: Test PASS (CI 43823427615) + Review APPROVE on feature/phase6b 6b514c8: follow-up inside a task topic resolves to that task's workflow |
| E2E-004 | Approval from Phone | 9, 13 | IN_PROGRESS | Phase 9: journey with fakes: /plan merge task → gate → Duyệt → one merge; reject/expiry → no merge (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| E2E-005 | Server Restart Recovery | 5, 12, 13 | IN_PROGRESS | Phase 5: fake-provider crash/restart/reclaim tests pass on 2ac779b; live evidence is Phase 12/13 |
| E2E-006 | Duplicate Webhook | 4, 13 | PENDING |  |
| E2E-007 | Worker Failure | 7a, 8, 13 | IN_PROGRESS | Phase 7a: fake-executor failure paths in CI (c85e9ca, CI 44009727911; live laptop check pending) |
| E2E-008 | Cross-Platform Response | 10, 13 | IN_PROGRESS | Phase 10: journey with fakes: `/plan` on a ClickUp task → 📤 → Đăng → exactly one ClickUp comment with the PR link (Test PASS (CI 44332870926, 2803 tests) + Review APPROVE on feature/phase10 code a507263 (tests 658559f); fakes only; live ClickUp checks U1–U3 pending; VERIFIED on merge) |
| E2E-009 | Policy Violation | 9, 13 | IN_PROGRESS | Phase 9: journey with fakes: rule deny → task blocked policy_denied, no side effect, decision recorded (Test PASS (CI 44363609327, 3014 tests) + Review APPROVE on feature/phase9 code 9bb65d5 (tests 3b8421e); fakes only; VERIFIED on merge) |
| E2E-010 | New Integration | 10, 13 | PENDING |  |
