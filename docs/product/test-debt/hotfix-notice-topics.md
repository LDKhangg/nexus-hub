# Test debt — hotfix/notice-topics

Status: OPEN.

- Code: plan `docs/plans/2026-11-18-hotfix-notice-topics.md`, merged into `develop` with `Merge hotfix/notice-topics` (`ca61f1e`).
- Owner decision (2026-11-18): skip the Tester and the Reviewer for this hotfix; CI must stay green.
- Proven: compile checks and the existing CI suite on the merged head (no regressions); live on the VPS: 5 workflow topics pruned, the command topic "Điều phối" created and set.
- Unproven (open):
  - TD-NT-1 (REQ-1, REQ-2): the "📌 Mở topic task" button and `/topic` as a reply create the task topic once (a second tap or reply reuses it); a notice without a ClickUp task has no button; the callback data stays within 64 bytes and is authenticated.
  - TD-NT-2 (REQ-3, REQ-4): a free-text reply to a task notice reaches the assistant with only the task ID as trusted context; messages in `telegram_command_topic` are handled exactly as in General and replies stay in that topic.
  - TD-NT-3 (REQ-5): `nh telegram topics prune` deletes only `workflow` topics of terminal executions, never task, fixed or active ones, prints counts only, is safe to run again, and removes the row of a topic already gone in Telegram; plus a Reviewer check of the hotfix.
- Pay first in the next Phase Brief (Phase 10 Batch A).
