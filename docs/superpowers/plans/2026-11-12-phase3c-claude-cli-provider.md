# Phase 3c: local Claude CLI as an LLM provider

- Date: 2026-11-12. Status: approved by the owner in chat ("dùng cá nhân… fill vào cli rồi trả lời nhận trả lời đó rồi router… ok rồi thì mới dám dùng key").
- Branch: `feature/phase3c` from `main`.

## Goal

The owner can run the hub with no API key at first. The hub calls the Claude
Code CLI already logged in on the host machine, in headless mode
(`claude -p`), and treats its answer like any other LLM response. Once the
owner trusts the hub, they switch to an API key by changing one setting.

The provider is generic. It sits behind the existing LLM interfaces, so
triage, the actor, the summary compressor and behavior runs all work with
either provider.

Lead probe (2026-11-12): the following invocation returned `structured_output`
matching the schema, with usage and `total_cost_usd`, in an empty working
directory:

```sh
claude -p --tools "" --no-session-persistence --strict-mcp-config --setting-sources "" \
  --model <id> --system-prompt <s> --output-format json --json-schema <schema>
```

The prompt went in on stdin.

## Owner decision recorded

The Phase 2 constraint "git is the only binary ever executed" gets one
exception. The configured Claude CLI binary may run, but only as an LLM
provider, under the rules below. Agent tools still run only git.

## Requirements

- REQ-C1: The setting `llm_provider` chooses `anthropic` (the default; current
  behaviour) or `claude_cli`. The following settings are validated:
  - `claude_cli_path`: an absolute path to an existing executable file;
  - `claude_cli_timeout`: default 120s;
  - `claude_cli_concurrency`: default 1, maximum 4.
  The server logs which provider it uses at startup, never any credential.
- REQ-C2: Every call runs the binary directly, with no shell, using exactly
  these flags:
  - `-p --output-format json --no-session-persistence --strict-mcp-config`;
  - `--setting-sources ""`;
  - `--tools ""`: no built-in tool can run;
  - `--model <model>`;
  - `--system-prompt <engine system prompt>`;
  - for structured calls, `--json-schema <schema>`.
  Further rules:
  - The user prompt and context go in on **stdin**, never in argv.
  - The child runs in a fresh empty temp directory, deleted afterwards.
  - The child gets a minimal environment: `HOME`, `PATH`, `LANG`, plus
    `XDG_CONFIG_HOME` / `XDG_DATA_HOME` if set. No `DATABASE_URL` and no other
    variables.
  - The child runs in its own process group. It is killed on timeout or on
    context cancel.
  - Output is capped at 4 MiB.
  - A semaphore enforces `claude_cli_concurrency`.
- REQ-C3: A structured call returns `structured_output`. Plain text calls
  return `result`. Usage (input/output tokens) and `total_cost_usd` are
  recorded like API usage. These are mapped to errors:
  - `is_error: true` → provider error;
  - a non-zero exit → provider error;
  - bad JSON → provider error;
  - a timeout → a timeout code;
  - a rate limit or login problem, from known subtypes or messages → a
    distinct code (`llm_unavailable` or `llm_unconfigured`), with no raw text
    in logs or Telegram.
  Retries follow the existing retry rules.
- REQ-C4: Tool calling for the Tier-2 actor is emulated:
  - The transcript (turns, tool results) and the tool definitions are rendered
    into the stdin prompt.
  - A JSON schema `{text, calls:[{id,name,input}], stop: end_turn|tool_use}`
    is requested.
  - The response is mapped to `ToolResponse`.
  - Tool names not in the request's tool list are dropped and noted.
  - The hub's executor still runs every tool. The CLI never runs a tool itself.
- REQ-C5: Prompts, context and answers are never logged, and never shown in
  errors or `inspect`. The secrets rules still apply: context passes through
  the redactor before it reaches the CLI, as it does for the API.
- REQ-C6: CLI `nh llm check` runs one tiny structured call through the
  configured provider and prints the provider, the model, ok or a code, and
  the latency. It never prints content.
- REQ-C7: `docs/runbooks/self-host-home.md` documents the switch: start with
  `claude_cli`, then move to `anthropic` with a key.

## Tasks

One Implementer.

| Task | Scope | REQ |
|---|---|---|
| C1 | Settings, provider factory, CLI runner, structured and plain calls, tool-call emulation, usage, error mapping, `llm check`, runbook | C1–C7 |

## Global constraints

- Phase 2 and Phase 3 constraints hold, except for the single binary
  exception above.
- No change to Anthropic API behaviour when `llm_provider=anthropic`.
- Never read or copy the CLI's credential files. The hub only executes the
  binary.

## Review focus

1. Can any input make the CLI run a tool, read project settings or MCP, or
   receive the prompt through argv?
2. Is the child's environment minimal, and is the child killed on timeout?
3. Can tool-call emulation run a tool the request did not offer?
4. Does any prompt or answer text reach logs?
