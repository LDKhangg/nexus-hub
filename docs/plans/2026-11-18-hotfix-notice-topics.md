# Hotfix: notices stay in General, topics on demand, command topic

Status: APPROVED by the owner 2026-11-18 (Telegram chat). Owner: skip Tester and Reviewer; CI must stay green.

## Why

Every mention run opened its own forum topic (`mention-report` had no `report.topic`, so it fell back to a task or WF-n topic): five WF topics for five notices. The owner wants notices in General, a topic only when he asks for one from the notice, PR approvals in the one shared `PR approve` topic (unchanged), and one fixed topic for commands. Config already changed live: `mention-report` v2 has `report.topic: general`.

PCC: FR-131 (destination resolution), FR-132 (thread replies), NFR-034 (docs).

## Requirements

- REQ-1: An owner notice sent to General (or any fixed topic) for a run whose subject is a ClickUp task carries a button "📌 Mở topic task". Tapping it creates the task topic if it does not exist (same name "<custom id> · <title>" and pinned task link as `/topic`), or reuses it, then replies with the "Mở topic" URL button. A notice whose run has no ClickUp task (for example a Chat channel message) has no such button. Tapping twice never creates two topics.
- REQ-2: `/topic` with no argument, sent as a reply to such a notice, does the same as the button. Without a task on the replied-to message it keeps today's usage reply.
- REQ-3: Free text sent as a reply to such a notice reaches the assistant with that ClickUp task ID added as trusted context (the task ID only, never the notice text as instructions), so "tạo topic", "làm task này" or "/plan"-like requests know the task. If this needs a large change, stop and report instead.
- REQ-4: Setting `telegram_command_topic` (a fixed topic name, empty by default = today's behaviour). Messages in that fixed topic are handled exactly as in General: commands, assistant, task-link jump, `/topic`, `/plan <TASK-ID>`; replies go to that topic. `nh telegram topics ensure --workspace-id <uuid> --name <fixed name>` creates the fixed topic if missing and prints its thread id (idempotent).
- REQ-5: `nh telegram topics prune --workspace-id <uuid>` deletes, in Telegram (deleteForumTopic) and in the store, every `workflow`-kind topic whose execution is in a terminal state; task, fixed and active-execution topics are never touched. Prints counts only (deleted, kept, failed); safe to run again; a topic already gone in Telegram still has its row removed.
- REQ-6: `examples/behaviors/mention-report.yaml` sets `report.topic: "general"`; the runbook explains the notice → topic flow, the command topic setting and prune.

## Constraints

AGENTS.md repo constraints. No new migration unless strictly needed. Never log message text, tokens or payloads. Callback data stays within Telegram's 64 bytes and is authenticated like existing buttons. Generic: no behavior name is hard-coded.
