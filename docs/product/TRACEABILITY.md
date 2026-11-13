# PCC-001 traceability matrix

Status of every PCC-001 requirement. Statuses follow PCC §10.1.2: `PENDING`, `IN_PROGRESS`, `VERIFIED`, `BLOCKED`. Only the Lead edits this file, and only from evidence: `VERIFIED` needs a Test Report `PASS` and a Review Report `APPROVE` on the same merged commit, cited in the Evidence column. "Phase" is the repo phase in `docs/product/ROADMAP.md`.

Snapshot: 2026-11-12. Phase 1 merged; Phase 2 under test and review. Nothing is `VERIFIED` against the PCC yet: Phase 1/2 work covers parts of the IDs marked `IN_PROGRESS`.

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
| FR-009 | Các sensitive actions yêu cầu quyền hoặc approval phù hợp. | 4 | PENDING |  |
| FR-010 | Platform Adapter Registry. | 4 | PENDING |  |
| FR-011 | Capability discovery. | 4 | PENDING |  |
| FR-012 | Inbound event normalization. | 4 | PENDING |  |
| FR-013 | Outbound action execution. | 10 | PENDING |  |
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
| FR-030 | Persistent conversations. | 6 | PENDING |  |
| FR-031 | Conversation-to-user binding. | 6 | PENDING |  |
| FR-032 | Conversation-to-workflow binding. | 6 | PENDING |  |
| FR-033 | Một conversation hỗ trợ nhiều workflows. | 6 | PENDING |  |
| FR-034 | Một workflow liên kết được với nhiều external resources. | 6 | PENDING |  |
| FR-035 | Context resolution cho follow-up messages. | 6 | PENDING |  |
| FR-036 | Có workflow reference ID để định danh tường minh. | 6 | PENDING |  |
| FR-037 | Nếu không rõ workflow đích, hệ thống phải hỏi lại. | 6 | PENDING |  |
| FR-038 | Hỗ trợ bổ sung instructions khi workflow đang chạy. | 6 | PENDING |  |
| FR-039 | Version hóa instruction updates. | 6 | PENDING |  |
| FR-040 | Hỗ trợ cancel, resume, status và list từ conversation. | 6 | PENDING |  |
| FR-041 | Không trộn context của hai workflow độc lập. | 6 | PENDING |  |
| FR-042 | Có cơ chế giới hạn và tóm tắt context khi cần. | 2, 6 | IN_PROGRESS | Phase 2: rolling summary + packet budget |
| FR-043 | Intent classification. | 2, 7 | IN_PROGRESS | Phase 2: Tier-1 triage categories |
| FR-044 | Task type classification. | 7 | PENDING |  |
| FR-045 | Task priority proposal. | 2, 7 | IN_PROGRESS | Phase 2: urgency in triage |
| FR-046 | Complexity estimation. | 7 | PENDING |  |
| FR-047 | Capability requirements. | 7 | PENDING |  |
| FR-048 | Execution plan generation. | 7 | PENDING |  |
| FR-049 | Worker selection recommendation. | 7 | PENDING |  |
| FR-050 | Task decomposition. | 7 | PENDING |  |
| FR-051 | Result evaluation. | 7 | PENDING |  |
| FR-052 | Next-action recommendation. | 7 | PENDING |  |
| FR-053 | Clarification request generation. | 7 | PENDING |  |
| FR-054 | Plan revision. | 7 | PENDING |  |
| FR-055 | Structured outputs và schema validation. | 2, 7 | IN_PROGRESS | Phase 2: triage JSON validated |
| FR-056 | Configurable reasoning/iteration budget. | 2, 7 | IN_PROGRESS | Phase 2: token budget, step limit |
| FR-057 | Worker registry. | 8 | PENDING |  |
| FR-058 | Worker capability profiles. | 8 | PENDING |  |
| FR-059 | Worker availability tracking. | 8 | PENDING |  |
| FR-060 | Worker task assignment. | 8 | PENDING |  |
| FR-061 | Isolated working directories. | 8 | PENDING |  |
| FR-062 | Tool permission enforcement. | 2, 8 | IN_PROGRESS | Phase 2: tiered tool policy executor |
| FR-063 | Execution timeout. | 2, 8 | IN_PROGRESS | Phase 2: tool timeouts |
| FR-064 | Cancellation support. | 8 | PENDING |  |
| FR-065 | Captured tool results. | 2, 8 | IN_PROGRESS | Phase 2: tool_runs audit with summaries |
| FR-066 | Task progress reporting. | 8 | PENDING |  |
| FR-067 | Worker result contract. | 8 | PENDING |  |
| FR-068 | Worker failure reporting. | 8 | PENDING |  |
| FR-069 | Worker execution iteration limit. | 2, 8 | IN_PROGRESS | Phase 2: Tier-2 step limit |
| FR-070 | Artifact references. | 8 | PENDING |  |
| FR-071 | Workspace cleanup và retention. | 8 | PENDING |  |
| FR-072 | Ngăn concurrent write conflicts đối với cùng repository/branch. | 8 | PENDING |  |
| FR-073 | Create workflow definition. | 5 | PENDING |  |
| FR-074 | Update draft workflow. | 5 | PENDING |  |
| FR-075 | Validate workflow schema. | 5 | PENDING |  |
| FR-076 | Publish workflow version. | 5 | PENDING |  |
| FR-077 | Enable/disable workflow. | 5 | PENDING |  |
| FR-078 | Define triggers. | 5 | PENDING |  |
| FR-079 | Define orchestration settings. | 5 | PENDING |  |
| FR-080 | Define worker constraints. | 5 | PENDING |  |
| FR-081 | Define notification rules. | 5 | PENDING |  |
| FR-082 | Define approval rules. | 5 | PENDING |  |
| FR-083 | Define retries/timeouts. | 5 | PENDING |  |
| FR-084 | Persist immutable published versions. | 5 | PENDING |  |
| FR-085 | Execution luôn tham chiếu một workflow version cụ thể. | 5 | PENDING |  |
| FR-086 | Có cơ chế xuất/nhập config dạng JSON/YAML. | 5 | PENDING |  |
| FR-087 | State machine với valid transitions. | 5 | PENDING |  |
| FR-088 | Persistent execution records. | 5 | PENDING |  |
| FR-089 | Step-level checkpoints. | 5 | PENDING |  |
| FR-090 | Event history. | 5 | PENDING |  |
| FR-091 | Retry policy. | 5 | PENDING |  |
| FR-092 | Cancellation. | 5 | PENDING |  |
| FR-093 | Resume/recovery. | 5 | PENDING |  |
| FR-094 | Execution timeout. | 5 | PENDING |  |
| FR-095 | Dead-letter hoặc failed-job inspection. | 5 | PENDING |  |
| FR-096 | Dependency-aware task progression. | 5 | PENDING |  |
| FR-097 | Không giữ worker khi đang chờ user. | 8 | PENDING |  |
| FR-098 | Optimistic locking/leases để tránh nhiều scheduler cùng claim một job. | 2, 5 | IN_PROGRESS | Phase 2: agent job leases + thread lock |
| FR-099 | Reconciliation sau process restart. | 5 | PENDING |  |
| FR-100 | Persistent job queue. | 5 | PENDING |  |
| FR-101 | Configurable per-user workflow concurrency. | 8 | PENDING |  |
| FR-102 | Configurable worker pool size. | 8 | PENDING |  |
| FR-103 | Worker capability matching. | 8 | PENDING |  |
| FR-104 | Priority scheduling. | 8 | PENDING |  |
| FR-105 | Resource conflict handling. | 8 | PENDING |  |
| FR-106 | Job leases và lease expiration. | 5 | PENDING |  |
| FR-107 | Worker heartbeat hoặc tương đương. | 8 | PENDING |  |
| FR-108 | Queue visibility. | 5 | PENDING |  |
| FR-109 | Backpressure khi quá tải. | 8 | PENDING |  |
| FR-110 | Worker allocation được audit. | 8 | PENDING |  |
| FR-111 | Rule schema có version. | 9 | PENDING |  |
| FR-112 | Rule validation. | 9 | PENDING |  |
| FR-113 | Policy evaluation trước các privileged actions. | 2, 9 | IN_PROGRESS | Phase 2: policy before tool calls (tools only) |
| FR-114 | Deny-by-default khi không đủ quyền. | 2, 9 | IN_PROGRESS | Phase 2: nothing allowed by default (tools only) |
| FR-115 | Approval-gated actions. | 9 | PENDING |  |
| FR-116 | Policy decision audit. | 2, 9 | IN_PROGRESS | Phase 2: tool_runs audit incl. denied |
| FR-117 | Rule management qua UI. | 9, 11 | PENDING |  |
| FR-118 | Không cho AI sửa system-level policy tùy tiện. | 9 | PENDING |  |
| FR-119 | Chống prompt injection từ webhook, issues, comments và repository content. | 2, 9 | IN_PROGRESS | Phase 2: sanitizer, policy is the boundary |
| FR-120 | Phân biệt trusted instructions với untrusted data. | 2, 9 | IN_PROGRESS | Phase 2: untrusted content marked in prompts |
| FR-121 | Generate approval request. | 9 | PENDING |  |
| FR-122 | Bind approval với user, execution, action và resource cụ thể. | 9 | PENDING |  |
| FR-123 | Deliver approval notification. | 9 | PENDING |  |
| FR-124 | Approve. | 9 | PENDING |  |
| FR-125 | Reject. | 9 | PENDING |  |
| FR-126 | Approval expiration. | 9 | PENDING |  |
| FR-127 | Prevent duplicate approvals. | 9 | PENDING |  |
| FR-128 | Resume workflow sau approval. | 9 | PENDING |  |
| FR-129 | Ghi lại actor và thời gian quyết định. | 9 | PENDING |  |
| FR-130 | Revalidate policy/action context trước khi thực thi sau approval. | 9 | PENDING |  |
| FR-131 | Destination resolution. | 10 | PENDING |  |
| FR-132 | Original thread replies. | 10 | PENDING |  |
| FR-133 | Cross-platform notifications. | 10 | PENDING |  |
| FR-134 | On-started notification policy. | 10 | PENDING |  |
| FR-135 | On-completed notification policy. | 10 | PENDING |  |
| FR-136 | On-blocked notification policy. | 10 | PENDING |  |
| FR-137 | On-failed notification policy. | 10 | PENDING |  |
| FR-138 | Notification deduplication. | 10 | PENDING |  |
| FR-139 | Transactional outbox. | 1, 10 | IN_PROGRESS | Phase 1: notification_outbox; Phase 2: outbound_actions |
| FR-140 | Delivery retry. | 1, 10 | IN_PROGRESS | Phase 1: notice retry with backoff |
| FR-141 | Delivery status tracking. | 1, 10 | IN_PROGRESS | Phase 1: delivery status in outbox |
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
| NFR-001 | Không mất acknowledged inbound events khi process restart. | 12 | PENDING |  |
| NFR-002 | Có crash recovery. | 12 | PENDING |  |
| NFR-003 | Có durable queues. | 12 | PENDING |  |
| NFR-004 | Có idempotency strategy. | 12 | PENDING |  |
| NFR-005 | Có graceful shutdown. | 2, 12 | IN_PROGRESS | Phase 1/2: runServices graceful shutdown |
| NFR-006 | Có database backup và restore đã được thử nghiệm. | 12 | PENDING |  |
| NFR-007 | Với local database khỏe mạnh, webhook handler có p95 dưới 500ms, không tính external provider processing. | 12 | PENDING |  |
| NFR-008 | Sau khi webhook được xác thực và ghi nhận, việc tạo execution không cần chờ LLM hoàn tất. | 1, 4 | IN_PROGRESS | Phase 1/2: ingest does not wait for LLM |
| NFR-009 | Ít nhất hai worker tasks độc lập có thể thực thi đồng thời. | 8 | PENDING |  |
| NFR-010 | Tối thiểu 100 workflow executions được lưu/truy vấn ổn định trong bài test MVP. | 12 | PENDING |  |
| NFR-011 | UI không bị block bởi workflow dài. | 11 | PENDING |  |
| NFR-012 | Có configurable resource quotas và backpressure. | 8, 12 | PENDING |  |
| NFR-013 | HTTPS cho public endpoints. | 12 | PENDING |  |
| NFR-014 | Authentication cho Admin UI. | 11 | PENDING |  |
| NFR-015 | Webhook authenticity verification. | 1, 4 | IN_PROGRESS | Phase 1: ClickUp signature verification |
| NFR-016 | Secrets encryption at rest và secret redaction. | 2 | IN_PROGRESS | Phase 2: encryption at rest + redactor |
| NFR-017 | Least privilege cho integrations. | 12 | PENDING |  |
| NFR-018 | Không expose management endpoints không xác thực. | 11 | PENDING |  |
| NFR-019 | Sandboxed/restricted worker execution. | 2, 8, 12 | IN_PROGRESS | Phase 2: hardened git-only runner |
| NFR-020 | Chống path traversal và command injection tại tool boundaries. | 2, 12 | IN_PROGRESS | Phase 2: path/argument checks in tools |
| NFR-021 | Audit privileged actions. | 2, 12 | IN_PROGRESS | Phase 2: tool_runs audit |
| NFR-022 | Prompt-injection defenses và trust-boundary checks. | 2, 12 | IN_PROGRESS | Phase 2: sanitizer + policy boundary |
| NFR-023 | Không cho workflow hoặc message nội dung không tin cậy chỉnh sửa system policy. | 9 | PENDING |  |
| NFR-024 | Có data retention và deletion controls. | 12 | PENDING |  |
| NFR-025 | Structured logs. | 3 | PENDING |  |
| NFR-026 | Correlation IDs. | 3 | PENDING |  |
| NFR-027 | Execution timelines. | 12 | PENDING |  |
| NFR-028 | Worker status và queue depth. | 12 | PENDING |  |
| NFR-029 | Error metrics. | 12 | PENDING |  |
| NFR-030 | LLM usage/cost tracking khi provider cung cấp dữ liệu. | 2, 12 | IN_PROGRESS | Phase 2: token usage per job |
| NFR-031 | Health/readiness endpoints. | 3 | PENDING |  |
| NFR-032 | Có cảnh báo hoặc notification cho stuck/failed workflows. | 12 | PENDING |  |
| NFR-033 | Có README. | 3 | PENDING |  |
| NFR-034 | Có architecture documentation. | every | IN_PROGRESS | docs/architecture.md predates PCC; ROADMAP.md added |
| NFR-035 | Có database migrations. | 3 | IN_PROGRESS | goose migrations 00001–00003 |
| NFR-036 | Có OpenAPI hoặc tài liệu API tương đương. | 11 | PENDING |  |
| NFR-037 | Có automated tests. | 3 | IN_PROGRESS | Go tests exist; CI missing (P3) |
| NFR-038 | Có CI pipeline. | 3 | PENDING |  |
| NFR-039 | Có deployment instructions. | 13 | PENDING |  |
| NFR-040 | Có recovery/runbook documentation. | 12 | IN_PROGRESS | runbooks/clickup-telegram-local.md (local only) |
| DEP-001 | Reproducible deployment bằng Docker Compose và/hoặc systemd cho worker runtime. | 13 | PENDING |  |
| DEP-002 | Environment configuration document. | 3, 13 | PENDING |  |
| DEP-003 | Secrets không commit vào repository. | 3, 13 | IN_PROGRESS | .env.example has no values; secrets in DB |
| DEP-004 | Database migrations tự động hoặc có bước deploy rõ ràng. | 3, 13 | PENDING |  |
| DEP-005 | Startup health checks. | 3, 13 | PENDING |  |
| DEP-006 | Graceful shutdown. | 13 | PENDING |  |
| DEP-007 | Persistent volumes. | 13 | PENDING |  |
| DEP-008 | Backup scheduling. | 13 | PENDING |  |
| DEP-009 | Logs rotation/retention. | 13 | PENDING |  |
| DEP-010 | Cấu hình secure remote access. | 13 | PENDING |  |
| DEP-011 | Có hướng dẫn nâng version và rollback deployment. | 13 | PENDING |  |
| DEP-012 | Có fresh-install test trên môi trường sạch. | 13 | PENDING |  |
| E2E-001 | Mobile Prompt to Pull Request | 10, 13 | PENDING |  |
| E2E-002 | Multiple Concurrent Workflows | 8, 13 | PENDING |  |
| E2E-003 | Update Running Workflow | 6, 8, 13 | PENDING |  |
| E2E-004 | Approval from Phone | 9, 13 | PENDING |  |
| E2E-005 | Server Restart Recovery | 5, 12, 13 | PENDING |  |
| E2E-006 | Duplicate Webhook | 4, 13 | PENDING |  |
| E2E-007 | Worker Failure | 8, 13 | PENDING |  |
| E2E-008 | Cross-Platform Response | 10, 13 | PENDING |  |
| E2E-009 | Policy Violation | 9, 13 | PENDING |  |
| E2E-010 | New Integration | 10, 13 | PENDING |  |
