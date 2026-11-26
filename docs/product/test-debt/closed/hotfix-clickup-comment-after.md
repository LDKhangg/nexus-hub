# Test debt — hotfix/clickup-comment-after (6b.2)

Status: CLOSED 2026-11-17 — TD-CA-1..3 paid in Phase 7a Batch A (see `docs/product/reports/2026-11-17-phase7a-report.md`).

- Code head: `80606b9` (`fix(clickup): accept string before/after in webhook history items`), merged into `develop` with the `Merge hotfix/clickup-comment-after` commit.
- Cause: the first live ClickUp webhook on the VPS (registered 2026-11-16, id `de2df88e-…`) answered `400` to every `taskCommentPosted` callback. ClickUp sends `"after": "<comment id>"` (a JSON string) in comment history items; `historyValue` only decoded an object, so `json.Unmarshal` failed with `clickup: malformed JSON`. Earlier tests used synthetic payloads with `after: null`, so the suite never saw the real shape. (Deliveries before that were also blocked at the Cloudflare edge by Bot Fight Mode, now off.)
- Owner decision (2026-11-16): fix now and deploy once CI is green, without a Tester round or a Review; tests later.
- Proven: `go build ./...`, `go vet ./...`, `gofmt` clean; the existing CI suite on `80606b9` (no regressions); a Lead-side reproduction with a docs-shaped payload went from `malformed JSON` to one event; live delivery after deploy (see the phase status note).
- Unproven (open):
  - TD-CA-1 (FR-018, FR-012): parser test with a signed, real-shaped `taskCommentPosted` payload (`before: null`, `after: "<id>"`, tag parts) → one `EventCommandPosted` with `tagged_user_ids`; and through the HTTP handler → `200`, not `400`.
  - TD-CA-2: `before`/`after` as number, bool, array or `{}` never fail decoding; status callbacks with such values still error `missing new status`; assignee additions without a user id are still skipped.
  - TD-CA-3: no Reviewer check of `80606b9` (the `UnmarshalJSON` never fails; non-object values give a non-nil empty value).
- Pay first in the next Phase Brief (Phase 7a Batch A).

## Closed: waived by the owner

2026-11-23: the owner closed stage 1 (GĐ1) and waived all open test debt
("skip luôn"). The open items in this file are not proven; they are closed as
waived, not as passed, and the PCC IDs they cover stay unverified in
`TRACEABILITY.md`.
