# Phase 4: People directory and report preferences

- Date: 2026-11-13. Status: **approved by the owner** ("tôi ok duyệt plan này", 2026-11-13, with owner-defined report templates added as REQ-9a).
- v2 changes the focus after the owner's review: Phase 4 must identify **the
  other people** on the platforms the owner connects. Today a report says
  "user 106845197"; it should say "anh Định". The owner also controls how
  reports read: short, and without lines like "✅ Đã thả reaction
  white_check_mark".
- Branch (when approved): `feature/phase4` from `main` (122536d).
- Roadmap: `docs/product/ROADMAP.md` Phase 4, re-scoped by the owner (see
  "Roadmap change"). Contract: `docs/product/PCC-001.md`.
- PCC IDs:
  - FR-001, FR-002, FR-005, FR-006 (identities and per-user settings);
  - FR-011 (capability discovery, people side);
  - FR-052 (useful notifications);
  - NFR-022 (untrusted names);
  - NFR-024 (retention and deletion);
  - NFR-026.
- Decisions:
  - D3: webhook, as today.
  - D4 (revised): one user can belong to many workspaces.
- Workflow: the Lead writes briefs; the opencode `implementer` writes the code;
  the `tester` and `reviewer` agents verify (AGENTS.md).

## Goal

1. **The hub knows who the people are.**
   - Every person the owner meets on a connected platform becomes a
     **contact**: a ClickUp user who DMs or tags the owner, a GitHub PR
     author, a Telegram sender.
   - A contact has a display name and one or more platform accounts.
   - The hub fills the directory automatically from the platforms. The owner
     can set an alias ("anh Định") and join accounts of the same person
     across platforms.
   - Reports, traces and model context use the contact's name, not the
     numeric id.
2. **Reports read the way the owner wants.** A per-user report-preferences
   table decides:
   - the style (`short`, `normal`, `detailed`);
   - which lines appear (reaction line, AI summary, links, PR list);
   - the language.

   The owner changes them from Telegram or the CLI, and the next report
   follows.

Example for a DM from anh Định, short style, reaction line off:

```
✅ Approved PR #1601 cho anh Định
AI: approve_request (0.94)
https://github.com/org/repo/pull/1601
```

## Requirements

### A. People directory (contacts)

- REQ-1 (FR-002, FR-005): New tables, both scoped to a workspace (D4):
  - `contacts`: id (UUID), workspace, `display_name` (the owner's alias),
    `note`, created/updated time.
  - `contact_accounts`:
    - platform `clickup|github|telegram`, external id, and the contact it
      belongs to;
    - the platform's own `platform_name` and `username`;
    - source `auto|manual`, `first_seen`/`last_seen`;
    - unique per (workspace, platform, external id).

  Emails are never stored.
- REQ-2 (FR-011): Auto-fill, with no extra API call on the hot path where
  possible:
  - ClickUp senders and taggers from the data the hub already reads:
    - chat channel members (E8);
    - comment `user.username` (E7);
    - task assignees and creators in webhook payloads.
  - GitHub PR authors when `github_pr_*` tools read a PR.
  - Telegram `from.username` and name.

  Each sighting upserts the account (`last_seen`, `platform_name`). A new
  account creates a new contact with no alias.
- REQ-3: Name resolution: alias, then `platform_name`, then `username`, then
  `<platform> <id>`.
  - Every user-set name goes through `CleanDisplayName`: no controls or bidi
    characters, at most 100 runes, mentions neutralised.
  - Names are untrusted labels (NFR-022). A name never chooses a target and
    never becomes an instruction.
- REQ-4: Owner commands:
  - CLI:
    - `nh contact list [--platform] [--search]`;
    - `nh contact show <id>`;
    - `nh contact alias <id|platform:external-id> "<name>"`;
    - `nh contact merge <id> <id>` (joins accounts of one person);
    - `nh contact split <account>`;
    - `nh contact forget <id>` (NFR-024: deletes the contact and its
      accounts).
  - Telegram, owner only:
    - `/contacts [search]`;
    - `/alias <platform:id> <name>`;
    - a button "Đặt tên" under any report whose sender has no alias, which
      asks for the name as a reply.
- REQ-5 (NFR-026): Run trigger data keeps only ids; the contact is resolved
  when a report, trace or model context is built. Renaming a contact
  changes later reports, not stored runs.
- REQ-6: Model context gets `sender_name` (and `channel_name`, E4) from the
  directory for chat, comment and PR runs. The `/team` lookup (E7) and
  channel members (E8) feed the directory instead of being separate caches.

### B. Report preferences

- REQ-7 (FR-006, FR-052): Table `report_preferences`:
  - keyed by (user, workspace), with optional per-behavior overrides;
  - `style`: `short|normal|detailed`, default `short` for the owner;
  - toggles: `show_reaction`, `show_ai_summary`, `show_links`,
    `show_pr_titles`, `show_trace_hint`;
  - `language`: `vi|en`, falling back to the profile.
- REQ-8: Every report the engine builds goes through one renderer that reads
  the preferences. This covers the run-level report (3d/3e), notify, FYI,
  digests and mention reports.
  - `short` is at most 4 lines: the outcome with the contact name, the
    verdict label and confidence, the link.
  - `normal` is today's content without technical codes.
  - `detailed` is today's full report.
  - Failures and skipped approvals always show one line with the reason,
    whatever the toggles say.
- REQ-9: Owner commands:
  - `nh report-prefs show|set [--behavior <name>] key=value...`;
  - Telegram `/prefs`: shows the current values with toggle buttons, and
    `/prefs short|normal|detailed`.

  Changes apply to the next report. Invalid keys and values are refused
  with the list of valid ones.

- REQ-9a (owner-defined templates): The owner can write the report text per
  report kind: `pr_approve`, `pr_skipped`, `verdict_unavailable`, `mention`,
  `notify`, `fyi`, `digest`, `failure`, scoped per user/workspace with an
  optional per-behavior override.
  - Templates use named placeholders only:
    - `{contact}`, `{contact_platform}`;
    - `{pr_number}`, `{pr_repo}`, `{pr_title}`, `{prs}`;
    - `{verdict}`, `{confidence}`, `{ai_summary}`;
    - `{outcome}`, `{reason}`;
    - `{task}`, `{channel}`, `{link}`;
    - `{behavior}`, `{run_id}`.
  - Optional lines start with `?{placeholder}` and are dropped when that
    value is empty.
  - No logic, no loops and no free code: a fixed substitution engine.
  - Values are cleaned and mention-neutralised before substitution. A
    template can never print a token, the DM text or a field that is not in
    the list.
  - Commands:
    - `nh report-template set <kind> [--behavior] --file <path>`, or `-`
      for stdin;
    - `nh report-template show|reset|preview <kind>`; preview renders a
      sample with fake values;
    - Telegram: `/template <kind>` shows the current template and a preview;
      replying with new text saves it after validation.
  - Rejected with the exact reason:
    - an unknown placeholder;
    - more than 1,000 characters;
    - more than 12 lines;
    - control characters.
  - Without a template, the style from REQ-7 applies. A failure or skip
    always keeps its reason line: if the owner's template has no
    `{reason}`, the renderer appends one.

### C. The owner's own accounts

- REQ-10 (FR-001, D4): Tables `users` and `workspace_members` (role
  `owner|member`). A migration creates the owner user and links them to:
  - every existing workspace, as `owner`;
  - the Telegram user id;
  - the ClickUp user id;
  - the GitHub logins of `nh github-account`, as contact accounts flagged
    `is_self`.

  "Is this the owner?" checks read this table. The current settings stay as
  the bootstrap until the migration has run. No manual step.

### E. Telegram assistant (owner, 2026-11-13 evening)

- REQ-12 (FR-043, FR-053, FR-055): the owner can write to the bot in plain
  words, and the hub uses the configured LLM (the owner runs `claude_cli`)
  to work out what to do.
  - Applies to owner free text that is not a command and not the reply to an
    open ask or template prompt. Other chats stay ignored.
  - One model call returns one JSON object, checked against a schema
    (one retry with the error, then a fallback): `intent` from a fixed list,
    `args`, a short `reply` and a `confidence`.
  - Intents map only to existing owner capabilities: `help`, `contacts`
    (search), `trace` (last or given run), `runs` (recent runs, short),
    `prefs_show`, `template_show`, `answer` (a plain reply, no action),
    `clarify` (ask one question back), and the writes `alias`,
    `prefs_style`.
  - Reads run at once through the same handlers as the slash commands.
    Writes are never done directly: the bot shows what it will do with
    "Làm" / "Huỷ" buttons, and only "Làm" runs the same handler as the slash
    command.
  - No provider, a model error, an invalid answer or low confidence: the
    bot says it did not understand and points to /help. Nothing runs.
  - The owner's text is never stored or logged; at most 6 assistant calls
    per minute per chat (more get the /help hint).
  - The assistant is generic: intents are hub capabilities, not personal
    flows.

### D. Docs

- REQ-11: Runbook sections (plus "Talk to the bot" for REQ-12):
  - name your contacts (CLI and Telegram);
  - merge accounts of one person;
  - choose your report style;
  - forget a contact.

## Tasks

| Task | Scope | REQ |
|---|---|---|
| P1 | Migrations and sqlc: `contacts`, `contact_accounts`, `report_preferences`, `report_templates`, `users`, `workspace_members`, owner bootstrap; Go interfaces `ContactResolver` and `ReportPrefs` (stubs) so P2 and P3 can start in parallel | 1, 7, 9a, 10 |
| P2 | Directory service: upsert from ClickUp, GitHub and Telegram sightings, name resolution, cleaning, model-context names (replaces the E7/E8 caches) | 2, 3, 5, 6 |
| P3 | Report renderer with preferences and owner templates (placeholder engine, validation, preview), used by run, notify, FYI, digest and mention reports | 7, 8, 9a |
| P4a | CLI `nh contact …`; Telegram `/contacts`, `/alias`, "Đặt tên" button | 4 |
| P4b | CLI `nh report-prefs …`, `nh report-template …`; Telegram `/prefs`, `/template` | 9, 9a |
| P5 | Runbook, examples updated to the new report style | 11 |
| F1 | Review #1 fixes R1–R4 | 5, 8, 9a |
| P6 | Telegram assistant: free text → LLM intent → read now or confirm-then-write | 12 |

## Roadmap change (owner, 2026-11-13)

The original Phase 4 items move to a later phase, **Phase 4b: event
gateway**:
- the generic `inbound_events` inbox and event contract v1 (FR-021–FR-028);
- the `PlatformAdapter` registry (FR-010, FR-012, FR-017);
- `/link` codes for other hub users and the allowlist (FR-004, FR-007,
  FR-008).

The Lead updates ROADMAP.md when this plan is approved.

## Global constraints

- Phase 2/3/3d/3e constraints hold.
- Names and usernames are personal data:
  - never in logs;
  - stored only in `contacts`/`contact_accounts`;
  - deletable with `forget`;
  - no emails, no avatars.
- Names never decide a target or an action; ids from trusted trigger data do.
- Existing behaviors keep working. With no preferences row, the owner gets
  `short` with every toggle on, except `show_reaction` off.
- Tests use fakes only, and DB tests use `nexus_storage_test` on loopback.

## Review focus

- Can a crafted platform name inject text or mentions into a report or the
  model context, or change a target?
- Do aliases stay per workspace? Does `forget` remove every copy?
- Do all report paths use the one renderer and the preferences, and do
  failures always show?

## Out of scope

- The event inbox, adapter registry and multi-user `/link` (Phase 4b).
- Syncing contacts back to any platform. Contact photos and emails.
