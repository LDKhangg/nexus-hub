# Báo cáo Phase 2 — Agent core, multi-tool, GitHub/Git, secret trong DB (chốt ngày 2026-11-10)

> Plan chi tiết (nguồn chính, tiếng Anh): `docs/superpowers/plans/2026-11-10-phase2-agent-core-tools.md`
> Nhánh: `feature/phase2` (tách từ `master` = `54aa048`, Phase 1 đã merge vào `main` + `master` và đã push).
> Trạng thái: **plan đã viết xong, chưa triển khai**. Bắt đầu triển khai vào ngày 2026-11-11.

## 1. Mục tiêu

Phase 2 làm **core cho chắc** để AI phối hợp được nhiều tool (GitHub, git, ClickUp, sau này thêm app/CLI khác), **trước** khi làm các tính năng reply. Nexus phải chạy được an toàn trên VPS.

```
Comment ClickUp
  → lọc (bỏ comment của bot; comment của owner chỉ lưu làm ngữ cảnh)
  → gom comment liên tiếp (8s, tối đa 30s, lưu trong PostgreSQL)
  → dựng ngữ cảnh đã sanitize
  → Tier-1 (Haiku): bỏ qua | báo tóm tắt | điều tra
  → (điều tra) Tier-2 (Sonnet) ReAct ≤ 5 bước, gọi tool qua executor có phân quyền
  → báo cáo một chiều về Telegram (kèm danh sách tool + số token)
```

## 2. Các quyết định đã chốt với user

| # | Nội dung | Kết quả |
|---|---|---|
| 1 | Auto-Pilot tắt mặc định | Đồng ý |
| 2 | Debounce lưu trong PostgreSQL | Đồng ý |
| 3 | Bảng outbox riêng `outbound_actions` | Đồng ý |
| 4 | Comment của owner chỉ lưu làm ngữ cảnh, không huỷ gì | Đồng ý |
| 5 | Commit local theo từng task, không push nhánh | Đồng ý |
| 6 | Reply/duyệt (nút Telegram, AI comment lên ClickUp, Auto-Pilot) | Dời sang phase sau |
| 7 | AI được gọi tool, kể cả tool ghi / merge / close, có cấu hình quyền chặt | Đồng ý |
| 8 | Không dùng `gh` CLI; GitHub gọi qua REST API, code dùng `git` | Đồng ý |
| 9 | Token GitHub: cả PAT và GitHub App | Đồng ý |
| 10 | Thêm bộ tool đọc code, sanitizer, các biện pháp giảm token | Đồng ý |
| 11 | **Shell trên VPS:** chỉ cho chạy `git`; chặn `sudo`, `ssh`, `sh`/`bash`, `docker`… | Đồng ý |
| 12 | **Git được phép:** clone, fetch, pull, checkout, reset, clean, commit, config, **push** | Đồng ý |
| 13 | **DB là nguồn cấu hình chính**, không dùng biến môi trường (secret mã hoá trong DB) | Đồng ý |
| 14 | Nghiệm thu thật, review độc lập, ra lệnh từ Telegram | Dời sang Phase 3 |

## 3. Bộ tool

| Cấp | Tool |
|---|---|
| read | `github_pr_view`, `github_pr_checks`, `github_pr_files`, `github_issue_view`, `clickup_get_task`, `code_list_files`, `code_search`, `code_read_file`, `code_outline`, `git_log`, `git_diff`, `git_blame`, `git_status` |
| write | `github_pr_comment`, `git_sync` (clone/fetch), `git_pull` (chỉ fast-forward), `git_checkout`, `git_commit` (gồm cả add), `git_config_set` (chỉ key trong allowlist), `git_push` (lên branch khớp mẫu `push_branches`, mặc định `nexus/*`) |
| dangerous | `github_pr_merge`, `github_pr_close`, `github_issue_close`, `git_reset_hard`, `git_clean`, và `git_push` lên `main`/`master`/base branch/default branch |

**Không bao giờ làm được:**
- force push, xoá branch qua push, mirror;
- `git clean -x`, `commit --amend`;
- sửa các config `core.*`/`credential.*`/`url.*`/`alias.*`/`remote.*.url`;
- approve review, workflow dispatch;
- chạy bất kỳ binary nào khác ngoài `git`.

**Giới hạn có chủ đích:** Phase 2 chưa có tool sửa file, nên `git_commit` chỉ commit những thay đổi đã có sẵn trong clone. Agent sửa code để phase sau.

## 4. Lớp an toàn

- **Phân quyền theo cấp:**
  - read: repo phải được cấu hình;
  - write: tool phải nằm trong `allow` của repo;
  - dangerous: thêm công tắc DB `allow_dangerous=true` và điều kiện riêng của tool; mỗi job tối đa 1 lần.
- **Clone do Nexus quản lý:** mọi lệnh git ghi hoặc phá huỷ chỉ chạy trong `/var/lib/nexus-hub/repos/<owner>/<repo>`, không bao giờ chạy trong repo bạn đang code.
- **Điều kiện riêng của merge:** PR không phải draft, đang mở, `head_sha` khớp, CI xanh, merge method được phép, base branch được liệt kê rõ. `main`/`master` không bao giờ được tự hiểu là cho phép.
- **Đúng account:**
  - PAT: gọi `GET /user` để kiểm tra login. Sai thì báo `account_mismatch`, không fallback sang account khác.
  - GitHub App: tự ký JWT, xin token ngắn hạn chỉ cho đúng repo và đúng quyền.
  - Token cho git đi qua biến môi trường riêng của tiến trình con, không bao giờ nằm trên argv, trong `.git/config` hay trong log.
- **Executor duy nhất cho mọi lần gọi tool:** ngân sách 12 lần gọi / 2 lần ghi / 1 lần dangerous mỗi job; timeout 30s (lệnh mạng 120s); output tối đa 6 KiB; ẩn secret; audit `tool_runs`; không lặp thao tác ghi khi job chạy lại; có chế độ `dry_run`.
- **Git được làm cứng:**
  - tắt `fsmonitor`, hooks, external diff, textconv, credential helper;
  - chỉ cho giao thức HTTPS tới `github.com`;
  - chặn file nhạy cảm (`.env`, `*.pem`, `secrets/`…);
  - kiểm tra path và ref chặt;
  - clone quá 2 GiB thì xoá.
- **Sanitizer:**
  - đầu vào: xoá ký tự điều khiển, zero-width, bidi;
  - đầu ra: ẩn secret theo giá trị thật và theo mẫu;
  - comment GitHub và commit message do AI viết: vô hiệu hoá `@mention`.

## 5. Giới hạn khi chạy trên VPS

- **Chỉ một binary là `git`**, và chỉ các subcommand trong allowlist; subcommand khác bị chặn trước khi chạy.
- **Mỗi tiến trình git:**

  | Giới hạn | Giá trị |
  |---|---|
  | CPU | 20s (lệnh mạng 90s) |
  | RAM | 1 GiB |
  | Dung lượng file ghi | 512 MiB |
  | Output | 1 MiB |
  | Độ ưu tiên | `nice` 10 |
  | Chạy song song | tối đa 2 |
  | Khoá theo repo | có |
  | Khi quá giờ | kill cả nhóm tiến trình |

  Biến môi trường của tiến trình con chỉ gồm vài biến cố định.
- **Mạng:** chỉ được tới `api.github.com`, `api.clickup.com`, `api.telegram.org`, `api.anthropic.com` (và `github.com` cho git). Chặn IP nội bộ/loopback, không theo redirect.
- **File systemd** `deploy/systemd/nexus-hub.service` (deploy thật để phase sau):
  - chạy bằng user riêng `nexus`, không có sudo;
  - `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`;
  - chỉ ghi được `/var/lib/nexus-hub`;
  - `MemoryMax=1G`, `CPUQuota=80%`, `TasksMax=64`;
  - không có capability nào; `SystemCallFilter=@system-service`;
  - master key đưa vào qua `LoadCredential`.

## 6. Cấu hình và secret nằm trong DB

- **Biến môi trường chỉ còn 2:**
  - `DATABASE_URL`: để kết nối DB;
  - `NEXUS_MASTER_KEY_FILE`: đường dẫn tới file khoá 32 byte, quyền 0600.
- **Bảng `credentials`:** lưu token Telegram, ClickUp, GitHub PAT, key của GitHub App, Anthropic key.
  - Mã hoá **AES-256-GCM**.
  - Mỗi bản ghi gắn chặt với workspace + tên + loại, nên copy sang chỗ khác sẽ không giải mã được.
  - Giá trị đã giải mã được cache 5 phút và tự động được ẩn khỏi mọi output.
- **Bảng `app_settings`:** model, timeout, ngân sách token, công tắc `allow_dangerous`, thư mục repos/tmp, giới hạn clone, debounce…; có giá trị mặc định và được kiểm tra hợp lệ.
- **Config tham chiếu secret bằng tên** (`bot_token_ref`, `token_ref`, `private_key_ref`…). Cấu hình `*_env` cũ của Phase 1 vẫn chạy cho tới khi chạy `migrate-secrets`.
- **CLI:**
  - `master-key init`
  - `secret set|list|delete`: secret nhập bằng prompt ẩn hoặc stdin, không bao giờ qua tham số dòng lệnh
  - `migrate-secrets`
  - `settings get|set|list`
  - `github-account add`
  - `github-repo set`
  - `github-check`
  - `enable-agent`
  - `inspect` (không in secret)

## 7. Giảm token

1. Tier-1 dùng model nhỏ, tối đa 300 token.
2. Nhánh "báo tóm tắt" không chạy Tier-2.
3. Tier-2 tối đa 1024 token mỗi bước; báo cáo ≤ 8 gạch đầu dòng, ≤ 1500 ký tự.
4. Tool trả text gọn, tối đa 6 KiB.
5. Kết quả tool cũ (cách 2 bước trở lên) được rút gọn trong vòng lặp.
6. Prompt caching cho system prompt và danh sách tool.
7. Đếm token cho mỗi job, ngân sách mặc định 60k input / 6k output. Số token hiện trên Telegram và trong `inspect`.

## 8. Thay đổi dữ liệu

Migration mới `00003_create_agent_core.sql` thêm 5 bảng: `agent_jobs`, `outbound_actions`, `tool_runs`, `credentials`, `app_settings`. Không sửa migration 00001/00002.

## 9. 19 task (TDD, mỗi task một commit local)

1. Parse và định tuyến quyết định Tier-1
2. Sanitizer
3. Config agent / tool / account / repo
4. Schema 00003 + ingest có debounce
5. Secret mã hoá + settings trong DB + resolver; chuyển Phase 1 sang dùng resolver
6. Dựng context packet
7. Tier-1 router + usage; Anthropic key lấy từ DB
8. Tool core: registry + executor phân cấp
9. Runner chỉ cho phép `git` + network guard
10. GitHub REST + PAT + GitHub App
11. Tool GitHub read/write, tool đọc ClickUp, store audit
12. Tool GitHub dangerous: merge / close
13. Tool đọc code bằng git
14. Tool git workspace: sync/pull/checkout/commit/config/push/reset/clean
15. LLM tool-calling + prompt caching
16. Tier-2 ReAct + ngân sách token
17. Agent worker + outbox Telegram
18. Rolling compressor
19. Wiring, CLI, file systemd, runbook, test end-to-end

**Kiểm chứng cuối:** `goose validate`, `make generate` chạy 2 lần không đổi, `gofmt`, `go vet`, `go test -race ./...` với PostgreSQL thật. Yêu cầu 0 skip.

## 10. Hạng mục để sau

- **Phase 3:**
  - Nghiệm thu thật. Cần một task ClickUp được phép dùng để test.
  - Review độc lập.
  - Owner ra lệnh từ Telegram, ví dụ "đăng bai bai lên channel general". Khi đó sẽ chốt: đăng ngay hay chờ gõ "ok", channel ở đâu, có cho `@mention` không.
- **Các phase sau:** reply/duyệt, AI comment lên ClickUp, Auto-Pilot, agent sửa code, GitHub làm nguồn webhook, pgvector, BYOI pool động, nhiều provider LLM, xoay vòng master key, deploy VPS thật.

## 11. Lưu ý môi trường

- Trong shell có các alias nguy hiểm: `git`→`lgtm`, `ss`→`git stash save --include-untracked`, `cp`→`git cherry-pick`, `mv`→`git merge`, `wc`→`git clean --dry-run`, `cat`→`bat`, `ls`→`eza`. Khi chạy script luôn dùng `command <tên lệnh>`.
- PostgreSQL local của app: container `nexus-phase1-postgres` (127.0.0.1:5433, DB `nexus_hub_dev`, đã migrate tới version 2). Bản ghi định tuyến Phase 1 đã tạo sẵn:
  - source `01a1c504-3854-7da3-9ab9-f733f37f1bf9`, đang tắt vì chưa bind webhook
  - nhóm Telegram `testhub`, chat_id `-5532282565`, bot `@nexus_hub0240_bot`
- Hiện `.env` đang giữ token Telegram và ClickUp. Sau Task 5 sẽ chạy `migrate-secrets` để chuyển các giá trị này vào DB.
- Test DB tạo tạm khi cần: DB tên `nexus_storage_test`, chỉ mở trên loopback, truyền vào qua `TEST_DATABASE_URL` (biến này chỉ dùng khi chạy test, server không đọc).
