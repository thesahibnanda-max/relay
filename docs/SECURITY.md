# Security

## Threat model

Relay runs as your user, on your machine, and connects terminals **you** started. It defends against:

1. **Other local users** reading or driving your agents.
2. **Web pages** in your browser reaching the daemon.
3. **A misbehaving or prompt-injected agent** abusing the messaging channel to attack another agent, you, or
   the tool's configuration.
4. **Relay changing how Claude/Codex behave** outside a Relay session.

It does not defend against another process running as *your own user* (which could read `~/.claude` too).

## Boundaries and protections

* **No network listener, unless you opt into a mesh.** The daemon listens only on a unix socket in a 0700
  directory (socket 0600). Connections are checked with `SO_PEERCRED`/`LOCAL_PEERCRED` on the server, and
  clients verify the daemon is their own uid (a socket path in a shared temp dir could otherwise be
  squatted). Any `Origin` header is refused, so a web page cannot use it. The admin API and the agent
  WebSocket share that socket. A daemon that never runs `relay session invite`, `session new --host` or
  `--join` never opens any other listener, full stop - see "Multi-machine mesh" below for exactly what
  changes once you do.
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

## Multi-machine mesh

Joining a session across machines (`internal/federation`, `internal/meshnet`) relaxes some of the boundaries
above; each change is deliberate and scoped to exactly the machines and sessions actually opted in.

* **Opt-in, per-session network exposure.** The first time this daemon runs `relay session invite`, `session
  new --host` or `--join`, it opens a listener reachable from the internet (via WireGuard, using
  `github.com/tailscale/tailcat` - no Tailscale account, no control plane). Nothing else ever triggers this;
  a daemon used only locally is exactly as closed as described above.
* **`--join=<blob>` is a bearer credential.** It embeds a session's peer address and its join secret - anyone
  who has it can join that session as a full participant. Treat it exactly like you would a session-invite
  link or a password: don't paste it into a public channel, and mint a fresh one (`relay session invite`)
  rather than reusing an old one if you're unsure who has seen it.
* **Peer identity is TOFU-pinned, not certificate-verified.** The first cryptographic identity seen for a
  peer in a session is recorded and never silently replaced; a machine that regenerates its identity key (or
  a genuine impersonation attempt) shows up as a distinct, visible new peer in `relay session peers` rather
  than being trusted as "the same machine." There is no out-of-band verification of who holds a given
  identity beyond that pinning - the join secret is what actually authorizes membership.
* **The join secret is stored in the clear locally** (unlike a resume token, which is only ever hashed): a
  peer needs to re-embed it in fresh invites it mints later, which a hash can't give back. It is protected by
  the same filesystem permissions as the rest of `~/.relay`, not by cryptography at rest.
* **DERP fallback is a real third-party dependency.** When two machines can't reach each other directly (most
  NATs), tailcat falls back to a public relay server it does not operate (`tailcat.dev` by default). That
  service only ever sees encrypted WireGuard traffic - never message content or terminal output - but its
  uptime is a genuine dependency for that fallback path; if you need to avoid it entirely, point
  `MeshDERPMapURL` at your own DERP map.
* **Guards still apply per hop.** Hop limit, rate limits, dedupe and TTLs (see "Bounded blast radius" above)
  are enforced by the *sending* agent's own daemon before a message ever leaves that machine, regardless of
  how many machines a reply chain eventually crosses.
* **A daemon that never joins a mesh is unaffected**, including at rest: no mesh identity file is created,
  no mesh tables gain rows, until a mesh command is actually used.

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
