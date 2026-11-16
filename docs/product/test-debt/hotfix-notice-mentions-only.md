# Test debt — hotfix/notice-mentions-only

Status: OPEN.

- Code head: see `fix(notices): option to notice only comments that tag the owner`, merged into `develop` with the `Merge hotfix/notice-mentions-only` commit.
- Cause: every ClickUp comment and status change on the watched lists became a Telegram notice (26 comment and 4 status notices on 2026-11-16 alone, none tagging the owner).
- Fix: the ClickUp source config key `notice_mentions_only: true` limits notices to comments from someone else that tag `owner_clickup_user_id`, directly (`tagged_user_ids`) or by a group mention (`tagged_group`, from group part types or `@everyone`/`@all`/`@followers`/`@assignees`… in the text). A comment that started a behavior run (mention-report) gets no notice. Without the key, behaviour is unchanged.
- Owner decision (2026-11-16): fix now, merge and deploy once CI is green, without a Tester round or a Review; tests later.
- Proven: `go build ./...`, `go vet ./internal/...`, `gofmt` clean; the existing CI suite (no regressions).
- Unproven (open):
  - TD-NM-1: with the key on, untagged comments, status changes, owner's own comments and comments that started a behavior run make no `notification_outbox` row; a comment tagging the owner or a group does; with the key off, nothing changes.
  - TD-NM-2: `tagsGroup` against a real ClickUp group mention payload (the part type ClickUp uses is unconfirmed; the text regex is the fallback) and no false positive on e-mail addresses (`a@all.com`).
  - TD-NM-3: no Reviewer check.
- Pay first in the next Phase Brief.
