*Inspired by [Iris](https://www.iris-tl.dev/), made by [psrth](https://github.com/psrth).*

# Relay

Relay lets AI coding agents in separate terminals work as one team. Run Claude Code and Codex
(or several of each) side by side, and one can hand work to another (*"tell codex to fix the failing
test"*), ask what another has done, and answer back, while every terminal still looks and behaves
exactly like the tool itself.

```
 terminal A                       terminal B
 $ relay claude orchestrator      $ relay codex developer --session=<id>
   --session=NEW                    
 > tell coder to add tests        [relay | from lead (orchestrator) | task | normal | msg 01J...]
                                  Add tests for parser.go ...
                                  • Called relay.relay_send(... "12 tests added")
 ● coder replied: 12 tests added
```

**Zero footprint.** Relay's powers exist only while a process runs under `relay`. It never writes to
`~/.claude`, `~/.codex`, or your project (no `.mcp.json`, `CLAUDE.md`, `AGENTS.md`, hooks or settings),
and never runs `claude mcp add` / `codex mcp add`. Everything is passed for that one launch (flags,
`-c` overrides, temp files in `~/.relay/run/<agent>/` that are deleted on exit). Run plain `claude` or
`codex` afterwards, or after uninstalling Relay, and they behave exactly as before.
`relay doctor` checks this, and an automated test enforces it.

Linux, macOS and WSL only (native Windows is not supported: `relay` says so and exits). The code still compiles on every platform so editors and tools stay clean. One static binary, no cgo.

**New to Relay?** Visit the [website](https://relay-sahib-nanda.vercel.app) for a quick overview, or read
[NOTICE.md](NOTICE.md) for a plain-language explanation of what it does, where it works, and what its limits are.

## Install

**macOS, Linux and WSL** (one line: picks the right build, checks it, and adds it to your PATH):

```sh
curl -fsSL https://relay-sahib-nanda.vercel.app/install.sh | bash
```

Then open a new terminal and run `relay doctor` to verify the installation.

* **Which build?** The script detects it for you. By hand: Mac with an Apple chip = `darwin_arm64`,
  Intel Mac = `darwin_amd64`, Linux/WSL = `linux_amd64` (Intel/AMD) or `linux_arm64` (ARM). WSL is just Linux.
* **Options** (set before `bash`): `RELAY_VERSION=v1.2.3` pins a version, `RELAY_INSTALL_DIR=/dir` changes
  where it goes (default `~/.local/bin`), `RELAY_NO_MODIFY_PATH=1` leaves your shell startup files alone.
  Example: `curl -fsSL .../install.sh | RELAY_VERSION=v1.2.3 bash`.
* **Manual install:** every [release](../../releases) lists the downloads and the step-by-step commands.
* **Remove it:** delete `~/.local/bin/relay` and the `# added by relay installer` line in your shell startup file.
* **From source** (needs Go): `go install github.com/thesahibnanda-max/relay@latest`, or `make build` for `./bin/relay`.

Relay wraps tools you already have (`claude`, `codex`) found on your `PATH`.

## Quick start

```sh
relay claude orchestrator --session=NEW --name=lead     # prints the session id
relay codex developer --session=<id> --name=coder       # in another terminal
```

Now just talk to Claude normally: *"ask coder to run the tests and tell me what fails."* Claude calls
`relay_send`; the request appears in Codex's terminal as a prompt; Codex answers with `relay_send`; the
answer appears in Claude's. You can also step in yourself: `relay send coder "stop, use the v2 API"`.

Without `--session`, `relay claude` just runs Claude as a private, solo session (recorded, but with no messaging features added).

## Commands

| Command | What it does |
|---|---|
| `relay <claude\|codex> [role] [--session=NEW\|<id> \| --join=<blob>] [--name=x [--resume\|--fresh]] [--approve-inbound] [--record=raw\|events\|off] [-- tool args]` | Run a tool as an agent. `--join` connects to a session on another machine (see [Multi-machine](#multi-machine)). Everything after `--` goes to the tool unchanged. |
| `relay ls [--all] [--session=<id>]` | Sessions and their agents (including ones joined from another machine). |
| `relay session new [--name=..] [--host]` / `end <id>` / `invite <id>` / `peers <id>` | Create / close a session, or mint (`--host`, `invite`) and inspect (`peers`) a multi-machine join. |
| `relay send <agent> <text> [--priority=low\|normal\|high\|interrupt] [--session=<id>]` | Message an agent yourself (`-` reads stdin). |
| `relay messages [--session=..] [--agent=..] [--state=..]` | What agents said to each other, and where each message is. |
| `relay approve [ls\|accept\|reject] [<id>\|all]` | Decide on messages held for `--approve-inbound` agents. |
| `relay gc [--older-than=30d] [--compress] [--dry-run]` | Clean up crashed-agent leftovers; forget idle sessions; zstd-compress old logs. |
| `relay doctor` | Health check: permissions, daemon, database, tools, and stray Relay files. |
| `relay daemon [status\|stop]` | The background service (starts on demand). |

**Roles** are `orchestrator`, `planner`, `developer`, `qa`, `reviewer`, or a path to your own `.md`
(with optional frontmatter: `can_interrupt`, `can_broadcast`). A role is delivered to the model once,
at launch, as part of its system prompt.

**Shim mode.** Symlink `claude` or `codex` to `relay` earlier on your `PATH` and typing `claude` runs
it under Relay, solo, with all arguments passed straight through.

## Multi-machine

Agents don't have to be on the same computer. Two (or more) Relay daemons connect directly over a private
WireGuard tunnel (via [tailcat](https://github.com/tailscale/tailcat) - no account, no server to run), so
agents on different machines can join the very same session and talk to each other exactly like a local one:

```sh
# machine A: create a session and mint an invite in one step
relay session new --host
#   01AB2C3D4E5F6G7H8J9K0MNPQR
#   from another machine, join with: relay <claude|codex> [role] --join=<blob>
relay claude orchestrator --session=01AB2C3D4E5F6G7H8J9K0MNPQR --name=lead

# machine B
relay codex developer --join=<blob> --name=coder
```

Already have a session running locally and want to invite someone into it? `relay session invite <id>`
mints a fresh `--join=<blob>` for it at any time; `relay session peers <id>` shows what this daemon knows
about the other machines in a session. A peer's identity is verified and pinned the first time it's seen,
and if a direct connection isn't possible it falls back to a public relay that only ever sees encrypted
traffic - see [docs/SECURITY.md](docs/SECURITY.md) for exactly what that means, and
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how it's built.

A crashed or killed `relay <tool>` process picks its identity back up automatically when relaunched with the
same `--session`/`--name` - no flag needed. Use `--fresh` to register as a new agent on purpose instead, or
`--resume` to fail loudly rather than silently falling back to a fresh registration.

## What the agents get

Each agent in a session gets these MCP tools (registered for that launch only):

`relay_whoami` · `relay_list_agents` · `relay_send` · `relay_inbox` · `relay_ack` · `relay_wait` · `relay_get_context`

`relay_get_context` reads another agent's recent conversation (last turns, its last answer, search)
without interrupting it. Secrets in that text are masked before they cross to another agent.

### How messages are delivered

A message is never lost, only delayed, and it is never typed into a permission dialog or on top of
text you are in the middle of writing.

| Target is... | Interrupt | High | Normal | Low |
|---|---|---|---|---|
| idle at the prompt | typed now | typed now | typed now | typed if nothing else waits |
| working (Claude) | Esc, then typed | at the next tool call, as hook context | as the turn ends (Stop-continue) | as the turn ends |
| working (Codex) | Esc, then typed | after the turn | after the turn | after the turn |
| showing a dialog / plan awaiting your decision | held | held | held | held |
| you have unsent text in the input box | held | held | held | held |

Priorities are `low`, `normal`, `high`, `interrupt`. Only roles with `can_interrupt` may interrupt
(otherwise it is downgraded to `high`). Long-waiting messages age upward.

### Safety valves

* **`--approve-inbound`**: messages to that agent wait until *you* approve them (`relay approve`, or
  press <kbd>Ctrl</kbd>+<kbd>\</kbd> then <kbd>a</kbd>/<kbd>r</kbd> in that agent's terminal). Agents cannot
  approve their own mail.
* **Loop guards**: a reply chain 8 deep is held for you (and again every 8); 20 messages/min per pair,
  60/min per sender; identical messages are coalesced; messages expire after an hour; you can't
  broadcast without `can_broadcast`.
* **Untrusted by design**: text from other agents is attributed with a header the sender cannot forge and
  the agent's briefing tells it to treat teammates' messages like any untrusted input.

## Files and configuration

Everything lives under `~/.relay` (override with `RELAY_HOME`), private to your user:

```
run/relayd.sock        the daemon's socket (0600)      data/relay.db     SQLite (sessions, messages, turns)
run/<agent id>/        per-launch files, removed on exit   data/raw/       raw terminal logs (zstd-compressed once closed)
log/relayd.log         daemon log                      sessions/         per-process local event logs
```

`--record=events` keeps structured events but no terminal bytes; `--record=off` keeps only presence.
Terminal logs contain everything typed, including secrets you type: use `--record=off` for sensitive work.

## Troubleshooting

* `relay doctor`: start here. It explains anything wrong and what to run.
* "daemon speaks a different protocol version": `relay daemon stop`, then retry.
* An agent seems to ignore a message: `relay messages --state=held` (waiting for approval?) and
  `relay ls` (is its state `dialog`?). Held messages are delivered when it is safe.
* Leftover files after a crash: `relay gc`.

## Development

```sh
make check        # gofmt + vet + tests under the race detector (what CI runs)
make short        # quick tests only
make fuzz         # every fuzz target for 10 s each
make cross        # linux/darwin x amd64/arm64 builds into ./dist
```

### Website (`ui/`)

A one-page site (problem, solution, demo screenshot, install) in plain HTML/CSS/JS under `ui/site/`. Node is only
needed for local preview and the tests.

```sh
cd ui
npm ci
npm run dev       # http://localhost:4173, copies ../install.sh into the site first
npm test          # Playwright: layout at 320-1440px, demo image, install.sh check, accessibility
npx playwright install chromium   # once, to download the test browser
```

The site serves its own copy of the installer at `/install.sh`. The root `install.sh` stays the single source of
truth: `npm run dev`, `npm run build` and `npm test` copy it to `ui/site/install.sh` (git-ignored), and a test
fails if the served copy ever differs. To host it, use any static host: run `npm run build` in `ui/` and publish
`ui/site`. On Vercel import the repo with **Root Directory = `ui`** and Framework Preset "Other": the root `vercel.json`
sets the build command (`node scripts/sync-install.mjs`), the output folder (`site`) and the headers, with paths
relative to `ui/`.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how it works and [docs/SECURITY.md](docs/SECURITY.md)
for the threat model. The end-to-end tests drive the real `relay` binary against a scripted fake tool;
opt-in checks against real Claude and Codex are described in the architecture notes.

## License

Relay is released under the [BSD 3-Clause License](LICENSE). The third-party modules linked into the
binary, and their licences, are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)
(regenerate with `make notices`).
