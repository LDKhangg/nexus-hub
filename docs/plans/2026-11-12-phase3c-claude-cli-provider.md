# Phase 3c: local AI CLIs (Claude Code, opencode) as LLM providers

- Date: 2026-11-12. Status: approved by the owner in chat ("dùng cá nhân… fill vào cli rồi trả lời nhận trả lời đó rồi router… ok rồi thì mới dám dùng key").
- Branch: `feature/phase3c`, rebased on `main` 987d08e (Phase 3d merged).
- Amended 2026-11-13 (v2): the owner wants **opencode with OpenAI models** as
  well ("dùng claude cli hoặc opencode nhưng mà model của openAI"). Added
  REQ-C8..C11 and task C2. v2 approved by the owner in chat ("ok", 2026-11-13); default opencode model for the owner: `openai/gpt-6-luna`.

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

### opencode provider (v2)

Lead probe (2026-11-13, opencode v2.2.7, OpenAI signed in with OAuth): in an
empty directory,

```sh
printf '<prompt>' | opencode run --model openai/gpt-6-luna --format json
```

read the prompt from stdin and printed newline-delimited JSON events:
`step_start`, `text` (`part.text` held the JSON answer the prompt asked for),
`step_finish` (`reason`, `cost`, `tokens.input/output`). Findings that the
requirements below answer:

- there is no `--json-schema` flag: structured output must be asked for in the
  prompt and checked by the hub;
- the default agent carries opencode's own system prompt and tools: the probe
  used about 16k input tokens for a two-line prompt;
- each run creates a session (`sessionID`);
- v2 talks to a background service unless `--standalone` is given.

- REQ-C8: `llm_provider` also accepts `opencode_cli`. Settings:
  `opencode_cli_path` (absolute path to an executable), `opencode_cli_timeout`
  (default 120s), `opencode_cli_concurrency` (default 1, max 4). When the
  provider is `opencode_cli`, the model settings (`triage_model`,
  `actor_model`, `summary_model`, a behavior's `model`) must be
  `provider/model[#variant]` ids (for example `openai/gpt-6-luna`); with
  `anthropic`/`claude_cli` they keep today's validation. `nh settings set
  llm_provider opencode_cli` refuses while a model setting does not fit, and
  says which.
- REQ-C9: Each call runs `opencode run --standalone --format json --model
  <model> --agent nexus-hub` directly (no shell), prompt and context on
  **stdin**, in a fresh empty temp directory deleted afterwards. The hub writes
  an `opencode.json` into that directory that defines the agent `nexus-hub`
  with the engine's system prompt, **every tool disabled and every permission
  denied**, no MCP servers. The child gets the same minimal environment as
  REQ-C2 (plus `XDG_*` so opencode finds its own sign-in), its own process
  group, kill on timeout/cancel, 4 MiB output cap, and the semaphore. If the
  probe for C2 shows a run still offers a tool or reads outside the temp
  directory, the Implementer stops and reports instead of shipping.
  **Amended 2026-11-13 (owner chose option (a), "a"):** on opencode v2.2.7 the
  built-in `browser` plugin keeps the `skill` tool offered to every agent.
  This is accepted under these conditions, which replace "every tool
  disabled" for opencode:
  - every permission is denied (`*` and `skill`), skill loading is denied, and
    no read, glob, grep, write, edit, shell, web or MCP tool is offered;
  - the child gets a hub-owned, empty `XDG_CONFIG_HOME` (so the owner's global
    opencode AGENTS.md, plugins and MCP config are never loaded); its
    `XDG_DATA_HOME` stays the owner's, where the sign-in lives;
  - **any `tool_use` event in the output makes the call a provider error**
    (`llm_error`, code noted as `cli_tool_use`); its text is not used;
  - the system prompt states that no tools exist;
  - runs use `--title nexus-hub` (no extra titling model call on content).
- REQ-C10: Output parsing: the answer is the concatenated `text` parts;
  usage comes from `step_finish.tokens`; `reason` other than `stop`, an
  `error` event, a non-zero exit, a timeout or bad JSON map to the REQ-C3
  codes (`llm_unavailable`, `llm_unconfigured` for a missing sign-in).
  Structured calls append the JSON schema and "reply with one JSON object
  only" to the prompt, then validate the answer against the schema; one
  repair attempt is allowed (the error, not the content, is fed back); a
  second failure is a provider error. Tool calling for the actor uses the
  same emulation as REQ-C4. The session the run created is deleted (or the
  run is made not to persist one, whichever the probe proves); no transcript
  of a hub call stays in opencode's history.
- REQ-C12 (from the C2 probe): `nh settings set` accepts several
  `key value` pairs and stores them in one transaction, so the owner can
  switch `llm_provider` and the model settings together; validation runs on
  the merged result and names every model setting that does not fit the
  provider. Behavior `model` and the profile's default model accept both id
  shapes at import; the run checks the shape against the provider and fails
  with `llm_unconfigured` on a mismatch.
- REQ-C11: `nh llm check` works for `opencode_cli` too and prints the input
  token count, so the owner sees the cost of the agent prompt. REQ-C5 (no
  content in logs) and REQ-C7 (runbook) cover opencode.

## Tasks

Two Implementers, in order.

| Task | Scope | REQ |
|---|---|---|
| C1 | Settings, provider factory, CLI runner, Claude CLI structured and plain calls, tool-call emulation, usage, error mapping, `llm check`, runbook | C1–C7 |
| C2 | opencode provider on the C1 runner: config file, agent without tools, NDJSON parsing, schema check and repair, session cleanup, model id validation, `llm check`, runbook | C8–C11 |

## Global constraints

- Phase 2 and Phase 3 constraints hold, except for the single binary
  exception above.
- No change to Anthropic API behaviour when `llm_provider=anthropic`.
- Never read or copy the CLI's credential files. The hub only executes the
  binary.

## Review focus

1. Can any input make either CLI run a tool, read project settings or MCP,
   or receive the prompt through argv? (opencode: the generated agent config.)
2. Is the child's environment minimal, and is the child killed on timeout?
3. Can tool-call emulation run a tool the request did not offer?
4. Does any prompt or answer text reach logs?
