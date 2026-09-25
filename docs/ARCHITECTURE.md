# Architecture

## The idea

MCP is *pull*: a model only sees a message if it calls a tool. But a busy agent must be *told* things
and a terminal UI has no inbox. Relay therefore uses three cooperating channels, each doing what it does best:

| Channel | Direction | Used for |
|---|---|---|
| **MCP tools** (`relay mcp`) | agent → Relay | send, reply, list agents, read inbox, get context |
| **Native signals** (Claude hooks, transcripts) | tool → Relay | exact busy/idle/dialog state; mid-turn delivery; proof of delivery |
| **PTY injection** | Relay → tool | waking an idle terminal: bracketed paste + Enter |

The fallback ladder is: hook delivery → typed at the prompt when idle → pull via `relay_inbox`.

## Processes

```
 terminal A                                    terminal B
 +------------------------------+              +------------------------------+
 | relay claude (agent)         |              | relay codex (agent)          |
 |  PTY host + single input mux |              |  same                        |
 |  VT screen tracker           |              |                              |
 |  state machine + local bus   |              |                              |
 |  control socket (unix)       |              |                              |
 |   ^ relay mcp   ^ relay hook |              |                              |
 +------|----------------------+              +--------------|---------------+
        | WebSocket over a unix socket                        |
        +--------------------------+--------------------------+
                                   v
                     relayd (one per user): registry, router, durable inboxes
                     SQLite (WAL) + raw log segments + admin API (CLI)
```

* **Daemon = durable router.** Owns sessions, agents, messages and the database. Knows nothing about
  terminal state.
* **Agent = local scheduler.** Only it knows the tool's live state, so *when* to type a message is
  decided there (`internal/bus`).
* **`relay mcp` / `relay hook`** are short-lived helpers the tool spawns; they talk to their own agent
  over a per-agent unix socket (`run/<agent>/ctl.sock`, 0600, owner-uid check plus a token file).
* Relay never breaks the tool: with the daemon down the tool still runs solo and events are spooled and
  resynced (agents reconnect with jittered backoff, restarting the daemon if needed).

## Transports: local daemon vs. global session

Everything above describes the **local** transport (`internal/link`): agents talk to `relayd` over a
per-user unix socket, and that daemon is the durable router. A session can instead be **global**
(`internal/globallink`): agents dial a central server over a WebSocket, and that server plays the
durable-router role instead of `relayd`. `internal/cli`'s `connect()` is the one place that decides which
transport a given `--session` value uses; everything above it (`Session`, `HandleCtl`, the MCP tools) only
ever sees the shared `collab.Link` interface and never knows which one it got.

A global session has no local daemon, no admin HTTP API, and no raw-terminal event replay (`Send` is a
deliberate no-op there - see `internal/globallink`'s package doc): it exists so two machines, not just two
terminals on one machine, can share a session. The official `relay` binary points this at one operator-run
server automatically (baked in at release build time, see `internal/cli/builtinserver.go`); a binary built
from source can point it at any server, including a self-hosted one (see `server/DEPLOY.md`).

Unlike a local session, a global session also isn't kept forever: the server runs a periodic idle-session
cleanup job (`server/package/cron`, alongside the existing TTL-expiry/disconnect-reaper sweep) that
permanently deletes a session - and every one of its agents and messages, in one transaction - once it's
been idle for `SESSION_MAX_AGE` (default 24h). See `server/DEPLOY.md`'s "Data retention" section.

## Messages

`queued → dispatched → injected → acknowledged → done`, with `held` (needs approval) in front, and
terminal exits `rejected`, `expired`, `undeliverable`. Transitions are forward-only compare-and-set in
SQLite. Delivery is **at-least-once**: the daemon pushes `deliver` frames and re-sends unconfirmed ones on
reconnect; the agent de-duplicates by message id. Chaos tests kill the daemon dozens of times mid-delivery
and require zero loss and zero duplicates.

*injected* means typed (or handed to a hook); *acknowledged* is set when the message's header is seen in
the tool's own transcript: hard evidence the model received it.

## Deciding when to type (`internal/bus`)

A pure decision table over the tool's state, evaluated on every change:

* `dialog`, `starting`, `unknown`, desynchronised screen, or no bracketed paste → **hold**.
* The user is typing, or has an unsent draft (tracked from their keystrokes) → **hold**. Relay never
  pastes onto your draft.
* `idle` → inject (low priority only when nothing else waits); one message at a time, then wait for the
  tool to react.
* `busy` → interrupt-priority presses Esc (once) then injects; everything else waits, or rides a hook.

All writes to the tool's stdin go through one mux: user bytes are never split by an injection, and an
injection only happens at a key boundary.

## State detection (`internal/state`, `internal/term`)

Signals are merged by confidence: hooks/transcripts (authoritative) beat the screen and output-activity
heuristics, with these rules: a dialog on screen always wins; an authoritative "busy" expires when the
terminal goes quiet (Esc fires no Stop hook); timeouts stop stale signals living forever. A headless VT
emulator (fed after the user's terminal is served, off the hot path) supplies bracketed-paste mode,
alt-screen and dialog text; if it falls behind it reports *desynced* and delivery goes conservative.

* **Claude**: per-launch `--settings` registers hooks (SessionStart, UserPromptSubmit, PostToolUse, Stop,
  Notification) as `relay hook <event>`. High-priority messages return as `additionalContext` after a tool
  call; the rest return as a Stop `block` so the turn continues instead of a new prompt being typed.
* **Codex**: the rollout file (`~/.codex/sessions/.../rollout-*.jsonl`) gives exact turn boundaries
  (`task_started` / `task_complete`), located by working directory and the Relay briefing in its developer
  message. A finished plan awaiting your decision is treated as a dialog.
* **Copilot CLI**: its per-session event log (`~/.copilot/session-state/*/events.jsonl`) gives turn
  boundaries (`assistant.turn_start` / `session.task_complete`), located the same way as Codex's rollout
  file but matched against the typed bootstrap message instead of a developer-role transcript entry (see
  below - Copilot has no flag to deliver the briefing any other way). This format is undocumented and
  explicitly marked unstable upstream; permission dialogs are recognised from the screen only.

## Per-launch registration (`internal/adaptor`)

Each adaptor turns "this agent, this session" into extra flags and files for one launch:

* Claude: `--mcp-config`, `--allowedTools mcp__relay`, `--settings`, `--append-system-prompt`.
* Codex: `-c mcp_servers.relay.*` (tools auto-approved), `-c developer_instructions=`.
* Copilot: `--additional-mcp-config @<file>` plus, separately, `--allow-tool=relay` - confirmed live
  that the config file's own `"tools":["*"]` only controls which tools the model is offered, not
  whether calling one prompts for approval; without `--allow-tool` every `relay_send`/`relay_whoami`
  call pops its own "Do you want to use this tool?" dialog. No flag exists to deliver a system prompt
  at all, so the briefing always arrives as a typed bootstrap message instead.

Verified additive: the user's own MCP servers, hooks and settings keep working. If the user passes their
own `--append-system-prompt`/`--settings`/`developer_instructions`/`--additional-mcp-config`, Relay does
not override them: it degrades (typed briefing / idle-only delivery) and says so. Non-interactive
invocations (`-p`, `exec`, `mcp list`, ...) run untouched.

## Conversations (`internal/transcript`, `relay_get_context`)

Read-only parsers normalise the tools' transcripts into turns (`user`, `assistant`, `tool_call`,
`tool_result`, `system`). Turns are uploaded as events (unless `--record=off`), stored in SQLite, and served
by the daemon through the redaction filter, newest first, capped at 24 KB.

## Storage (`internal/store`, `internal/daemon`)

SQLite (WAL, pure-Go driver). Only the daemon opens it: one writer connection, many readers. Raw terminal
bytes are not in SQLite: append-only JSONL segments per agent (16 MB), zstd-compressed once closed, with
a per-agent size quota. `relay gc` prunes idle sessions and compresses leftovers.

## Testing

Unit tests per package; deterministic end-to-end tests that run the real binary against a scripted fake TUI
(`testdata/fakeagent`): delivery, approval, drafts, hooks, transcripts, zero footprint; chaos and soak tests
in `internal/link`; fuzz targets for the input parser, the injection sanitiser, the envelope decoder, the
transcript parsers, redaction and the MCP server. Real-Claude/Codex/Copilot verification is done by hand in
an isolated tmux server: use `RELAY_HOME` under `/tmp`, run in an already-trusted directory, type prompts
with `tmux send-keys -l` followed by a separate Enter. Copilot's `events.jsonl` format is unstable
upstream, and its own CLI flags change often, so re-verify after any Copilot CLI upgrade the same way.
