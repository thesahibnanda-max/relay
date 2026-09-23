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

## Per-launch registration (`internal/adaptor`)

Each adaptor turns "this agent, this session" into extra flags and files for one launch:

* Claude: `--mcp-config`, `--allowedTools mcp__relay`, `--settings`, `--append-system-prompt`.
* Codex: `-c mcp_servers.relay.*` (tools auto-approved), `-c developer_instructions=`.

Verified additive: the user's own MCP servers, hooks and settings keep working. If the user passes their
own `--append-system-prompt`/`--settings`/`developer_instructions`, Relay does not override them: it
degrades (typed briefing / idle-only delivery) and says so. Non-interactive invocations (`-p`, `exec`,
`mcp list`, ...) run untouched.

## Conversations (`internal/transcript`, `relay_get_context`)

Read-only parsers normalise both tools' transcripts into turns (`user`, `assistant`, `tool_call`,
`tool_result`, `system`). Turns are uploaded as events (unless `--record=off`), stored in SQLite, and served
by the daemon through the redaction filter, newest first, capped at 24 KB.

## Storage (`internal/store`, `internal/daemon`)

SQLite (WAL, pure-Go driver). Only the daemon opens it: one writer connection, many readers. Raw terminal
bytes are not in SQLite: append-only JSONL segments per agent (16 MB), zstd-compressed once closed, with
a per-agent size quota. `relay gc` prunes idle sessions and compresses leftovers.

## Multi-machine mesh (`internal/federation`, `internal/meshnet`)

Agents on **different machines** join the same session over a control-plane-free WireGuard tunnel
(`github.com/tailscale/tailcat`, not a Tailscale account) - a true N×N mesh, not a hub: every daemon that
learns of a peer eventually dials it directly, so no single machine's presence is load-bearing for anyone
else's connectivity. Entirely opt-in and additive: a daemon that never runs `relay session invite` or
`--join` never opens a listener, never touches `~/.relay/mesh`, and every local-only code path (the message
state machine above, `--session=<ULID>`) is completely unmodified.

**The design problem** is keeping N daemons' views consistent without real distributed consensus. The
answer is **single-writer ownership**: each daemon's SQLite stays authoritative only for the agents actually
connected to *it*; every other fact about a session (another daemon's roster, a message it owns) is either
gossiped read-only or mirrored, never written by more than one daemon. This means "keep the newer version"
is always correct by construction - there is never a concurrent writer to reconcile.

* **Transport (`internal/meshnet`)** is the *only* package that imports `tailcat`, isolating a
  no-stability-guarantee dependency to one file-sized blast radius. A daemon's own identity (a WireGuard
  key pair) is persisted so its address survives a restart; a fake in-memory transport backs every test, so
  CI never touches real tailcat/DERP.
* **Wire protocol (`internal/proto/mesh.go`)** has its own version counter, separate from the agent-facing
  protocol - no agent process ever sees a mesh frame. Every kind of mesh exchange (join handshake, roster
  resync, a message handoff, a receipt) is one short-lived connection carrying one request frame and one
  reply frame; there is no persistent daemon-to-daemon link to keep alive.
* **Joining** exchanges a `MeshHello`/`MeshWelcome` gated by a join secret (`--join=<blob>` embeds it - treat
  a blob like a bearer credential, not something to paste publicly). A peer's identity is **TOFU-pinned**: the
  first cryptographic identity seen for a peer is recorded, and a machine that regenerates its key becomes a
  *new*, visible peer rather than silently replacing the old one.
* **Roster gossip** replicates a read-only cache of every other daemon's agents (`mesh_agents`), versioned by
  each agent's own `last_seen_at` - a free monotonic counter, since only that agent's owning daemon can ever
  bump it. A name collision between two daemons' agents (both registered before gossip crossed) is resolved
  deterministically: agent ids are ULIDs, so comparing them (lexicographically smaller = created first)
  gives every daemon the same answer independently, and only the losing agent's *own* daemon ever renames it.
* **Cross-daemon messages** get a row on both daemons sharing one id: `origin='local'` on the recipient's
  daemon is fully authoritative and runs the unmodified state machine above; `origin='mirror'` on the
  sender's daemon holds the same immutable fields so hop-counting, `reply_to` and `relay wait` never need a
  live network round trip - only `state`/`rev` are updated later by an asynchronous, rev-gated receipt.
  Hand-offs are idempotent by id, so an at-least-once resend after a reconnect never duplicates a row.
  Sender-side guards (rate limits, dedupe, TTL) are enforced entirely by the sending agent's own daemon,
  since it is the sole point that agent's outbound traffic ever originates from.
* **Fan-out and healing**: joining fans out to every peer the seed already knew about (repeating until a
  round discovers nothing new), so a session becomes fully connected without depending on the seed staying
  up. A periodic resync (piggybacked on the daemon's existing message-expiry sweep) retries any unreachable
  peer and flushes anything still owed to it, so a network blip - or a whole daemon process restarting -
  heals on its own once the peer is reachable again, with no human action.
* **Agent identity resume** (`~/.relay/identities/<session>__<name>.json`) is orthogonal to the mesh: it lets
  a crashed/relaunched `relay <tool>` process pick its same identity back up on whichever single machine it
  runs on, using the daemon's pre-existing resume-token mechanism.

See `docs/SECURITY.md` for the DERP-fallback disclosure and what changes in the threat model once mesh
features are actually used.

## Testing

Unit tests per package; deterministic end-to-end tests that run the real binary against a scripted fake TUI
(`testdata/fakeagent`): delivery, approval, drafts, hooks, transcripts, zero footprint; chaos and soak tests
in `internal/link`; fuzz targets for the input parser, the injection sanitiser, the envelope decoder, the
transcript parsers, redaction and the MCP server. The mesh is tested the same way one layer out: multi-hub
fan-out, name-collision resolution, and partition/heal all run against an in-memory fake transport (never
real tailcat/DERP), including real daemon restarts (a fresh `daemon.Server` reopening the same on-disk
identity and store) to prove healing survives more than a network blip. Real-Claude/Codex verification is
done by hand in an isolated tmux server: use `RELAY_HOME` under `/tmp`, run in an already-trusted directory,
type prompts with `tmux send-keys -l` followed by a separate Enter.
