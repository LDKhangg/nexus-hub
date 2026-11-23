# Test debt: Phase 12 backup and restore (REQ-13)

- Phase: 12 (reliability, security and operations hardening)
- Branch: feature/phase12   Recorded at head: 765c9aa   Date: 2026-11-20
- Decision: owner, 2026-11-20, option C ("c"): skip the backup and restore
  drill in Phase 12 and carry it as debt.

## Why

The plan's owner decision O1 was "use Supabase's built-in backups". The
project is on the Supabase Free plan. Supabase documents daily backups only
for Pro, Team and Enterprise projects and recommends that Free projects export
their own data with `supabase db dump` or `pg_dump`
(https://supabase.com/docs/guides/platform/backups). O1 therefore cannot be
met as written, and the owner chose not to add an own backup job now.

## Unproven items

| Item | REQ | PCC IDs | What is missing |
|---|---|---|---|
| D12-1 | REQ-13 | NFR-006, DB criteria (backup and restore) | No scheduled database backup exists for the production database, and no restore has been tried. |
| D12-2 | REQ-13, REQ-16 | NFR-040 | The recovery runbook has no tested "restore the database" procedure; the master key backup is not described with a tested restore. |

## How to close

Either upgrade the Supabase project to a plan with daily backups and run one
restore drill into a scratch target, or add an own `pg_dump` job (for example
a systemd timer on the VPS with an off-site copy) and restore one dump into a
disposable local PostgreSQL, then start the hub against it with the master
key. Record the date and result in a Test Report.

## Status

- D12-1: open
- D12-2: open (the rest of REQ-16 is still in Phase 12 scope)

## Closed: waived by the owner

2026-11-23: the owner closed stage 1 (GĐ1), dropped Phase 13 and waived this
debt ("bỏ nợ test đi"). D12-1 and D12-2 are not proven; they are closed as
waived, not as passed. The PCC IDs they cover (NFR-006, NFR-040 restore part)
stay unverified in `TRACEABILITY.md`.
