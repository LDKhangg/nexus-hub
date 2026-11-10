# Báo cáo Phase 2 — Agent core & multi-tool (chốt ngày 2026-11-10)

> Plan chi tiết (nguồn chính, tiếng Anh): `docs/superpowers/plans/2026-11-10-phase2-agent-core-tools.md`
> Nhánh: `feature/phase2` (tách từ `master` = `54aa048`, Phase 1 đã merge vào `main` + `master` và đã push).
> Trạng thái: **plan đã viết xong, chưa triển khai**. Bắt đầu triển khai vào ngày 2026-11-11.

## 1. Mục tiêu

Khi có comment ClickUp, Nexus-Hub xử lý theo đúng luồng trong `ARCHITECTURE.md`. Phase 2 tập trung làm **core cho chắc** để phối hợp được nhiều tool (GitHub, git, ClickUp, sau này thêm các app/CLI khác), **trước** khi làm các tính năng reply.

```
Comment ClickUp
  → lọc (bỏ comment của bot, comment của owner chỉ lưu làm ngữ cảnh)
  → gom comment liên tiếp (8s, tối đa 30s, lưu trong PostgreSQL)
  → dựng ngữ cảnh đã sanitize (tóm tắt + 3 tin gần nhất + batch mới)
  → Tier-1 (Haiku): bỏ qua | báo tóm tắt | điều tra
  → (điều tra) Tier-2 (Sonnet) ReAct ≤ 5 bước, gọi tool qua executor có phân quyền
  → báo cáo một chiều về Telegram (kèm danh sách tool + số token)
```

## 2. Các quyết định đã chốt với user

| # | Nội dung | Kết quả |
|---|---|---|
| 1 | Auto-Pilot tắt mặc định | Đồng ý |
| 2 | Bộ gom comment (debounce) lưu trong PostgreSQL, không dùng RAM/Redis | Đồng ý |
| 3 | Bảng outbox riêng `outbound_actions`, giữ nguyên code Phase 1 | Đồng ý |
| 4 | Comment của owner trên ClickUp **không** huỷ gì, chỉ lưu làm ngữ cảnh | Đồng ý (sau khi user hỏi "sao chắc") |
| 5 | Commit local theo từng task trên `feature/phase2`, không push | Đồng ý |
| 6 | Reply/duyệt (nút Telegram, AI comment lên ClickUp, Auto-Pilot) | **Dời sang phase sau** |
| 7 | AI được gọi tool, **kể cả tool ghi / merge / close**, có cấu hình quyền chặt | Đồng ý |
| 8 | Không dùng `gh` CLI. GitHub gọi qua REST API, đọc code bằng `git` | Đồng ý |
| 9 | Token GitHub: **cả** fine-grained PAT **và** GitHub App | Đồng ý |
| 10 | Thêm bộ tool đọc code, sanitizer, các biện pháp giảm token | Đồng ý |
| 11 | Nghiệm thu thật (live) và review độc lập | Dời sang Phase 3 |

## 3. Bộ tool trong Phase 2

| Cấp | Tool |
|---|---|
| read | `github_pr_view`, `github_pr_checks`, `github_pr_files`, `github_issue_view`, `clickup_get_task`, `code_list_files`, `code_search`, `code_read_file`, `code_outline`, `git_log`, `git_diff`, `git_blame` |
| write | `github_pr_comment` |
| dangerous | `github_pr_merge`, `github_pr_close`, `github_issue_close` |

**Cố ý không làm:** xoá branch, force push, approve review, sửa file, workflow dispatch, release, tool kiểu "chạy lệnh tuỳ ý".

## 4. Lớp an toàn

- **Phân quyền theo cấp.**
  - read: repo phải được cấu hình.
  - write: tool phải nằm trong `allow` của repo.
  - dangerous: cần đủ 3 công tắc — có trong `allow` của repo, env `AGENT_ALLOW_DANGEROUS=1`, và qua được điều kiện riêng của tool. Mỗi job tối đa 1 lần dùng tool dangerous.
- **Điều kiện riêng của merge:** PR không phải draft, đang mở, `head_sha` khớp, CI xanh, merge method được phép, base branch được liệt kê rõ ràng. `main`/`master` không bao giờ được tự hiểu là cho phép.
- **Đúng account.**
  - PAT: gọi `GET /user` để kiểm tra login khớp với config. Sai thì báo `account_mismatch` và không bao giờ fallback sang account khác.
  - GitHub App: tự ký JWT, xin token ngắn hạn chỉ cho đúng repo và đúng quyền; file key phải có quyền `0600`.
- **Executor duy nhất cho mọi lần gọi tool:** ngân sách 12 lần gọi và 2 lần ghi mỗi job, timeout 30s, output tối đa 6 KiB, ẩn secret, audit `tool_runs` cho cả lần bị chặn.
- **Không lặp thao tác ghi:** tool ghi được ghi nhận trước khi chạy. Job chạy lại sẽ không ghi lần hai; trường hợp không rõ đã ghi hay chưa thì báo `unknown_outcome`, không chạy lại.
- **Chế độ `dry_run`:** xem AI *định* làm gì mà không làm thật.
- **`git` được làm cứng:**
  - chỉ đọc nội dung đã commit, file chưa commit như `.env` không đọc được;
  - chặn path nhạy cảm và path traversal;
  - tắt `fsmonitor`, external diff, textconv, hooks;
  - env tối thiểu; quá giờ thì kill cả nhóm tiến trình.
- **Sanitizer:**
  - đầu vào: xoá ký tự điều khiển, zero-width, bidi;
  - đầu ra: ẩn secret theo giá trị thật và theo mẫu (token GitHub/Anthropic/Telegram/AWS, private key, mật khẩu trong URL Postgres);
  - comment GitHub do AI viết: vô hiệu hoá `@mention`.

## 5. Giảm token

1. Tier-1 dùng model nhỏ, tối đa 300 token output.
2. Nhánh "báo tóm tắt" không chạy Tier-2.
3. Tier-2 tối đa 1024 token mỗi bước; báo cáo ≤ 8 gạch đầu dòng, ≤ 1500 ký tự.
4. Tool trả text gọn thay vì JSON thô.
5. Kết quả tool cũ (cách 2 bước trở lên) được rút gọn trong vòng lặp.
6. Dùng prompt caching cho system prompt và danh sách tool.
7. Đếm token cho mỗi job, có ngân sách mặc định 60k input / 6k output. Số token hiện trên Telegram và trong `inspect`.

## 6. Thay đổi dữ liệu và cấu hình

- **Migration mới `00003_create_agent_core.sql`:** thêm bảng `agent_jobs`, `outbound_actions`, `tool_runs`. Không sửa migration 00001/00002.
- **Config:** thêm object `agent` trong config của ClickUp source, gồm các tool được bật, `dry_run`, `max_writes_per_job`, `github.accounts` và `github.repos` (account, `local_path`, `allow`, `merge_methods`, `merge_base_branches`, `require_checks_green`).
- **Env mới:** `TRIAGE_MODEL`, `ACTOR_MODEL`, `SUMMARY_MODEL`, `AGENT_POLL_INTERVAL`, `TOOL_TIMEOUT`, `AGENT_MAX_INPUT_TOKENS`, `AGENT_MAX_OUTPUT_TOKENS`, `AGENT_ALLOW_DANGEROUS`, `GIT_PATH`, cùng các env token/key mà config tham chiếu tới.
- **CLI mới:** `enable-agent`, `disable-agent`, `github-account add`, `github-repo set`, `github-check`. Lệnh `inspect` được mở rộng.

## 7. 17 task (TDD, mỗi task một commit local)

1. Parse và định tuyến quyết định Tier-1
2. Sanitizer
3. Config agent / tool / account / repo
4. Schema 00003 + ingest có debounce bền vững
5. Dựng context packet
6. Tier-1 router + settings + đếm token
7. Tool core: registry + executor phân cấp
8. Exec runner được làm cứng
9. GitHub REST client + PAT + GitHub App
10. Tool GitHub read/write, tool đọc ClickUp, store audit
11. Tool dangerous: merge / close
12. Bộ tool đọc code bằng `git`
13. LLM tool-calling + prompt caching + usage
14. Tier-2 ReAct + rút gọn vòng lặp + ngân sách token
15. Agent worker + outbox Telegram
16. Rolling compressor
17. Wiring, CLI, runbook, test end-to-end

**Kiểm chứng cuối:** `goose validate`, `make generate` chạy 2 lần không đổi, `gofmt`, `go vet`, `go test -race ./...` với PostgreSQL thật. Yêu cầu 0 skip.

## 8. Câu hỏi còn mở (cần trả lời trước hoặc khi bắt đầu triển khai)

**Chiều ngược lại: ra lệnh từ Telegram**, ví dụ "đăng *bai bai mọi người* lên channel general". Plan hiện **chưa** hỗ trợ việc này. Nếu thêm vào Phase 2, cần:

- Telegram long-poll, chỉ nhận lệnh từ `owner_user_ids` trong đúng nhóm đã cấu hình.
- Mỗi lệnh thành một agent job, chạy Tier-2 với các tool đã cấp quyền và báo kết quả về Telegram.
- Tool ghi mới, ví dụ `clickup_chat_post`: chỉ đăng vào channel có trong allowlist, ngân sách ghi, dry-run, sanitizer.

Cần user quyết định:

1. Có thêm phần này vào Phase 2 không?
2. Bot đăng luôn rồi báo lại, hay phải gõ "ok" sau khi xem dry-run?
3. "Channel general" nằm ở đâu: ClickUp Chat, Telegram, Slack...?

## 9. Hạng mục để sau

- **Phase 3:** nghiệm thu thật (cần một task ClickUp được phép dùng để test), review độc lập.
- **Các phase sau:** reply/duyệt bằng nút Telegram, AI comment lên ClickUp, Auto-Pilot, GitHub làm nguồn webhook, agent tự `git fetch`, agent sửa code, pgvector RAG, BYOI/mã hoá credential, nhiều provider LLM, deploy VPS.

## 10. Lưu ý môi trường

- Trong shell có các alias nguy hiểm: `git`→`lgtm`, `ss`→`git stash save --include-untracked`, `cp`→`git cherry-pick`, `mv`→`git merge`, `wc`→`git clean --dry-run`, `cat`→`bat`, `ls`→`eza`. Khi chạy script luôn dùng `command <tên lệnh>`.
- PostgreSQL local của app: container `nexus-phase1-postgres` (127.0.0.1:5433, DB `nexus_hub_dev`, đã migrate tới version 2). Bản ghi định tuyến Phase 1 đã tạo sẵn:
  - source `01a1c504-3854-7da3-9ab9-f733f37f1bf9`, đang tắt vì chưa bind webhook
  - nhóm Telegram `testhub`, chat_id `-5532282565`, bot `@nexus_hub0240_bot`
- Test DB tạo tạm khi cần: DB tên `nexus_storage_test`, chỉ mở trên loopback, truyền vào qua `TEST_DATABASE_URL`.
