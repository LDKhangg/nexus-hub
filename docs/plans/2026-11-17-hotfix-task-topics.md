# Hotfix: task topic names, pinned task link, remembered repo

Owner request (2026-11-17): a ClickUp task topic must read
`FC-xxxx · <task title>` (custom task id first), carry a link that opens the
task, and remember the repository so `/task <request>` in that topic works
without naming the repo again. One topic follows one task; its updates and
questions go there.

PCC: FR-131, FR-132 (Telegram part), FR-060 (worker task assignment UX).

## Requirements

- REQ-1: A new ClickUp task topic is named `<custom id> · <title>` when the
  task has a custom id (for example `FC-6583 · M01.f02.us03.s01 Login`); with
  no custom id it keeps `<task id> · <title>`; with no readable task it keeps
  `<task id>`. Names stay sanitized and at most `MaxTopicNameBytes`.
- REQ-2: Right after a ClickUp task topic is created, the bot posts one
  message in it, `<custom id or task id> · <title>`, with a URL button
  "Mở task ClickUp" to the task URL from ClickUp, and pins it. A pin failure
  (no admin right, API error) logs a fixed message and is not retried; the
  message stays. The link message is posted at most once per topic.
- REQ-3: `nh telegram topics rename --workspace-id <uuid>` renames every
  ClickUp task topic that has a thread to the REQ-1 name
  (`editForumTopic`) and posts + pins the REQ-2 message where it was never
  posted. Output: counts only (renamed, linked, failed); no names or titles.
  Running it again changes nothing new.
- REQ-4: In a ClickUp task topic, `/task <request>` without an owner/repo
  uses the repository of the last local task for the same ClickUp task in
  that workspace. An explicit registered owner/repo still wins and becomes
  the remembered one. With no history and several repos: today's usage
  reply. Outside a topic, `/task <TASK-ID> <request>` uses the same memory.
- REQ-5: ClickUp notices and behavior messages for a task with a topic keep
  going to that topic (no regression; prove with a test).

## Constraints

- AGENTS.md repo constraints. New migration only if a column is needed
  (for example `telegram_topics.link_message_id`); never edit old ones.
- Topic names, titles and URLs are never logged or stored beyond what exists.
- No new secret paths; the ClickUp read uses the existing task reader.
- Tests: CI only, fakes only.

## Review focus

Duplicate link messages on retries or concurrent creates; rename on closed
topics; repo memory picking a repo that is no longer registered (must fall
back to the usage reply); 128-byte limit with multibyte titles.
