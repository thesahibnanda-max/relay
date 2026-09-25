# Security

## Threat model

Relay runs as your user, on your machine, and connects terminals **you** started. It defends against:

1. **Other local users** reading or driving your agents.
2. **Web pages** in your browser reaching the daemon.
3. **A misbehaving or prompt-injected agent** abusing the messaging channel to attack another agent, you, or
   the tool's configuration.
4. **Relay changing how Claude/Codex behave** outside a Relay session.
5. **A global session's server** (Relay's own hosted one, or a self-hosted alternative) seeing message
   traffic — relevant only if you use `--session=NEW` or a join token instead of `--session=NEW_LOCAL`.

It does not defend against another process running as *your own user* (which could read `~/.claude` too).

## Boundaries and protections

* **No network listener for local sessions.** The daemon listens only on a unix socket in a 0700 directory
  (socket 0600). Connections are checked with `SO_PEERCRED`/`LOCAL_PEERCRED` on the server, and clients
  verify the daemon is their own uid (a socket path in a shared temp dir could otherwise be squatted). Any
  `Origin` header is refused, so a web page cannot use it. The admin API and the agent WebSocket share that
  socket. This is `--session=NEW_LOCAL`/a bare-ULID session's whole story — see the next point for what
  changes with a global one.
* **Global sessions dial out, by design.** `--session=NEW` and a join token (`<ulid>@host:port`) are the one
  deliberate exception to the point above: message bodies, agent names and roles are sent to a remote
  server (`internal/globallink`) so two machines, not just two terminals on one machine, can share a
  session. The officially distributed binary always forces TLS (`wss://`) for this and is locked to one
  operator-run server (`internal/cli/builtinserver.go`); a self-hosted server should terminate TLS too (see
  `server/DEPLOY.md`). If you don't want any of this, `--session=NEW_LOCAL` never leaves the unix-socket
  boundary above.
* **Private files.** `~/.relay` is 0700; database, logs and raw terminal logs are 0600. Per-launch
  directories are created and verified (owned by you, not a symlink, mode 0700) before use.
* **Agents act only as themselves.** Identity comes from the connection, not from message fields. A
  resume token (stored only as a hash) is needed to reclaim an agent.
* **Messages are untrusted input.** Bodies are stripped of all control bytes before typing (so a message
  can never close the bracketed paste or send escape sequences; this invariant is fuzzed); message headers
  cannot be forged (`[relay |` in a body is defanged, roles/tools/names are restricted to a plain charset,
  and delivery confirmation only trusts a real header line). Relay **never auto-approves** a tool
  permission dialog and never types into one. The model's briefing tells it to treat teammates like any
  untrusted input.
* **Human-only approval.** `--approve-inbound` holds mail until a person approves it; there is no MCP tool
  for approving, so an agent cannot approve its own mail. The in-terminal chord only fires at a key boundary
  and never inside a paste.
* **Bounded blast radius.** Hop limit (8), per-pair and per-sender rate limits, an RPC rate limit per agent,
  duplicate coalescing, TTLs, 32 agents per session, 32 KB message bodies, per-agent raw-log quota, bounded
  read/write buffers, slow peers are disconnected.
* **Redaction.** `relay_get_context` masks API keys, tokens, JWTs, private keys, credentials in URLs and
  `secret=...` style assignments before text crosses to another agent. It is regex-based: a safety net, not
  a guarantee.
* **Zero footprint.** Relay never writes tool config or project files (see README); `relay doctor` scans
  for stray Relay registrations, and an automated test hashes the config dirs around a full session.

## Things to be aware of

* Raw terminal logs contain **everything typed and printed**, including passwords you type into the
  terminal. Use `--record=off` (or `events`) for sensitive sessions, and `relay gc --older-than=…` to forget old
  ones. Files are private to your user but not encrypted.
* Anything an agent tells another agent is visible to you in `relay messages`, and is stored.
* Another agent's text is attributed but cannot be *verified*; an agent that has been prompt-injected
  can ask its teammates for things. Keep permission prompts on for actions that matter (Relay will not
  answer them for you).
* Transcripts and rollouts are only ever *read*.

## Reporting a vulnerability

Please report privately to the maintainers rather than opening a public issue.
