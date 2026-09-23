# Notice: what Relay is and what you should know

This is a plain-language guide to Relay: what it does, how you use it, where it works, and where it
does not. For the technical details see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and
[docs/SECURITY.md](docs/SECURITY.md). Licence: [LICENSE](LICENSE) (BSD 3-Clause). Licences of the
libraries inside the program: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

---

## Where does it work?

Be aware of the difference between "built and tested" and "should work".

| Platform | Status |
|---|---|
| **WSL** | The main test environment. Every feature has been run here, including live with real Claude Code and real Codex. |
| **Linux (regular)** | Very likely fine, because WSL is Linux. Not yet run on a non-WSL Linux machine. |
| **macOS** | It compiles and passes the code checks for macOS, but **it has never been run on a Mac**. It should work; treat the first run as a test. The macOS-specific part (checking who is on the other end of a connection) is untried. The automated CI is set up to test on macOS but has not run yet, because the project is not on GitHub. |
| **Windows (native)** | Not supported. `relay` prints "use WSL" and exits. (The code still compiles for Windows so editors and tools do not show errors.) |

Two more caveats:

* **Tool versions.** Relay depends on how the tools behave: their hook formats, their log-file formats and
  what their screens look like. It was built and verified against **Claude Code 2.1.278** and **Codex
  0.155.1**. If a tool changes in an update, parts of Relay may need adjusting.
* **Claude's permission prompts.** They were never seen on screen during testing, because the test machine's
  Claude runs in "bypass permissions" mode. Relay has protection for them, but it was only tested against
  Codex's real prompts.

---

## What Relay is

Normally you run Claude in one terminal and Codex in another, and they cannot talk to each other.
**Relay is a layer that lets AI coding assistants in separate terminals work like a team.**

You launch each assistant through `relay`, and each terminal looks and behaves exactly as before. The
difference is that the assistants can now send each other messages ("Codex, please write tests for this",
"Claude, what did you decide?") and get answers back.

Think of a small office where each person works in their own room. Relay adds an internal phone system, a
message tray outside each room that is only emptied when the person is not mid-sentence, a switchboard
operator, and a logbook of what was said.

---

## How you use it

```
relay claude orchestrator --session=NEW --name=lead     (terminal 1)
relay codex developer --session=<id> --name=coder       (terminal 2)
```

* **Session**: a shared "room" that the assistants join. `NEW` creates one and prints an ID that others use
  to join it.
* **Name**: each assistant gets a name such as `lead` or `coder`. Leave it out and Relay makes one up (like
  `brave-fox`). Names are unique within a session, and a few words (`user`, `all`, `relay`, `me`, `self`,
  `none`, `new`) are reserved.
* **Role**: a job description given to the assistant once, at startup. Built-in roles are `orchestrator`
  (delegates work), `planner`, `developer`, `qa` and `reviewer`. You can also point to your own `.md` file
  describing a role, with optional settings such as whether that role may interrupt others or broadcast to
  everyone.
* **Without a session**: `relay claude` just runs Claude normally, as a private session that is recorded but
  has no messaging features.
* **Passing options to the tool**: anything after `--` goes to Claude or Codex unchanged, for example
  `relay claude -- --model sonnet`.

Then you talk to your assistant normally: *"Ask coder to run the tests and tell me what fails."* It handles
the rest.

---

## What the assistants can do (their seven tools)

Each assistant gets these abilities, and only while running under Relay:

1. **See who they are** (`relay_whoami`): their name, role and the session ID. If you ask "what is the
   session ID?", they can answer.
2. **See the team** (`relay_list_agents`): who is in the session, their role and tool, and what each is
   doing (idle, busy, waiting on a prompt).
3. **Send a message** (`relay_send`): to one teammate by name, to "all" (if their role allows), or to "the
   one with role qa". It returns immediately; the assistant does not freeze waiting. A reply can automatically
   go back to whoever asked.
4. **Read their inbox** (`relay_inbox`): read waiting messages right now instead of waiting for delivery.
5. **Mark a message handled** (`relay_ack`).
6. **Wait for an answer** (`relay_wait`): up to 45 seconds, only when they truly cannot continue without it.
7. **Read a teammate's recent conversation** (`relay_get_context`): the last few turns, the latest answer,
   the turns from the last 10 minutes, or a search. This does not interrupt the teammate at all. Passwords,
   API keys, tokens and similar secrets in that text are blacked out first, and very long output is trimmed
   (newest kept).

---

## How messages get delivered

The hard problem is that a terminal assistant has no mailbox. Relay solves it by delivering at the safest
moment:

* **If the assistant is idle**, the message is typed in as if a person typed it, with a header showing who it
  is from, for example `[relay | from lead (orchestrator) | task | normal | msg 01J...]`.
* **If it is busy (Claude):**
  * *High priority* messages are slipped in right after its next step, as extra context.
  * *Normal and low* messages are delivered just as it finishes, so it simply keeps going instead of having a
    new prompt typed. (Claude labels these "Stop hook error" in its screen. That is only its wording.)
* **If it is busy (Codex)**, the message waits until the current task ends.
* **Interrupt priority** presses Escape first, then delivers. Only roles that are allowed to interrupt can do
  this; otherwise the message is downgraded to "high".
* **It never types into a permission question** ("Allow this command?") or into a plan that is waiting for
  your decision.
* **It never types over something you are in the middle of writing.** If you have unsent text, it waits, and
  only treats a draft as abandoned after 10 minutes.
* Messages are **never lost, only delayed.** Priorities are low, normal, high and interrupt. Messages that
  wait a long time gradually move up in priority. Several "low" notes are bundled into one.

Relay knows what an assistant is doing from the tool's own signals (Claude's "hooks" and Codex's log file),
backed up by reading the screen and noticing whether output is flowing.

Each message moves through visible stages: queued, delivered to the assistant's scheduler, typed in, seen by
the model (confirmed from the tool's own transcript), done. It can also end as rejected, expired or
undeliverable, and the sender is told when that happens.

---

## Safety controls for you

* **`--approve-inbound`**: an assistant started with this holds every incoming message until *you* approve it.
  * Approve with `relay approve` (list, accept, reject), or press **Ctrl+\\** then **a** (accept) or **r**
    (reject) in that assistant's terminal. The chord only works in that mode and never inside pasted text.
  * Your terminal beeps when something is waiting.
  * Assistants can **never approve their own mail.**
* **Loop guards:**
  * A back-and-forth chain 8 messages deep pauses for you, and again every 8 after that.
  * Limits: 20 messages a minute per sender-and-recipient pair and 60 per sender.
  * Identical messages within 30 seconds are merged into one.
  * Undelivered messages expire after an hour.
  * Broadcasting to "all" needs a role that allows it.
* **You can join in:**
  * `relay send coder "stop, use the v2 API"` sends your own message (it skips approval and rate limits).
  * `relay messages` shows everything that was said and where each message stands.
* **Assistants are told to treat teammates' messages like any untrusted input**, and a message body cannot
  forge a header to pretend to be someone else.

---

## Recording and privacy

* Relay records terminal activity in its own private folder (`~/.relay`, readable only by you).
* `--record=raw` keeps everything (the default), `--record=events` keeps structure but not the terminal
  text, and `--record=off` keeps almost nothing (and turns off the conversation sharing described above).
* **Warning:** recordings include anything typed, including passwords you type into the terminal. They are
  **not encrypted**. Use `--record=off` for sensitive work.

---

## The "zero footprint" promise

Relay's abilities exist **only while a process runs under `relay`**. It never edits your Claude or Codex
settings, never adds files to your projects, and never runs commands like `claude mcp add`. Everything is
handed over for that one launch and deleted afterwards. Plain `claude` or `codex` behaves exactly as before,
even if you uninstall Relay.

* An automated test enforces it, `relay doctor` checks it, and it was confirmed against a real configuration.
* If you pass your own system-prompt or settings options to the tool, Relay does not override them. It falls
  back to a gentler method and tells you.
* For non-interactive commands (`claude -p`, `codex exec`, `mcp list`, and similar), Relay steps aside
  entirely.
* The tools' own history (Claude transcripts, Codex logs) will of course contain what happened during a Relay
  session. That is the tool's own record, not a Relay setting.

---

## The commands

| Command | What it does |
|---|---|
| `relay claude ...` / `relay codex ...` | Run an assistant (see above). |
| `relay ls` | Show sessions and who is in them. |
| `relay session new` / `relay session end <id>` | Create or close a session. |
| `relay send <name> <text>` | Send a message yourself (`-` reads the text from input). |
| `relay messages` | See the message history, with filters for session, agent and state. |
| `relay approve` | List, accept or reject held messages. |
| `relay gc` | Clean up crash leftovers. Add `--older-than=30d` to forget old sessions, `--compress` to shrink old logs, `--dry-run` to preview. |
| `relay doctor` | Health check: permissions, background service, database, tools installed, and a scan for stray Relay files. |
| `relay daemon status` / `relay daemon stop` | Control the background service (it starts itself when needed). |
| `relay version` | Show the version. |
| Shim mode | Symlink `claude` or `codex` to `relay` earlier on your PATH and typing `claude` runs it under Relay, on its own. |

---

## Reliability behind the scenes

* **A background "switchboard" (the daemon)** keeps a database of sessions, assistants and messages. It
  starts on demand.
* **If it crashes or restarts,** assistants reconnect on their own and nothing is lost or duplicated. This was
  tested with 300 messages while the daemon was restarted about 40 times, and with 8 assistants exchanging 960
  messages.
* **If an assistant vanishes** (its terminal is killed), people who message it are told it is not connected,
  and after 15 minutes it is marked gone so senders learn about it.
* **Slow or stuck connections are dropped** rather than freezing everything.
* **Limits:** 32 assistants per session, a cap on request rate, a 32 KB size limit per message, and a size cap
  on stored terminal recordings (1 GB per assistant).
* **Cleanup:** temporary files are removed on exit, and leftovers from a crash are collected by `relay gc`.

## Security in short

Relay only listens on a private local channel (no network port, so websites cannot reach it) and rejects
other users on the machine. Files are private to you. Message text is stripped of control characters so
nobody can sneak commands into another terminal, and names and roles are limited to plain characters. See
[docs/SECURITY.md](docs/SECURITY.md).

---

## What is included for developers

* A [README](README.md), an architecture guide and a security guide.
* A `LICENSE` (BSD 3-Clause, copyright Sahib Nanda) and a third-party notices file.
* A `Makefile`: `make check` runs the full tests, `make fuzz` stress-tests the parsers, `make cross` builds
  for other systems, `make notices` regenerates the third-party notices.
* Automatic testing and release setup on GitHub: every pull request is tested, and every merge to `main`
  publishes a release with builds for macOS and Linux (WSL uses the Linux build).
* A project website in `ui/` (plain HTML, CSS and JavaScript; no framework) with its own browser tests. It uses the
  Inter and JetBrains Mono fonts, both under the SIL Open Font License; their licence files are in
  `ui/site/assets/fonts/`.
* Tests from tiny units up to running the real program against a fake assistant, plus stress and chaos tests.

---

## What it does not do, and known gaps

* No dashboard or replay screen (planned as a later idea, not built).
* A crashed assistant cannot be "resumed" by restarting it. A restarted one is a fresh assistant.
* Recordings are not encrypted, and secret-hiding is pattern-based, so it can miss unusual secrets.
* Disk-full situations are not tested.
* Search inside a teammate's conversation is a simple text match, not a smart full-text search.
* The project is **not in git yet**, so there is no history or backup of the work.
* If your Claude runs in "bypass permissions" mode, a teammate's message could lead to commands running with
  no confirmation. Keep that in mind for sessions that matter, and consider running Claude with normal
  permission prompts for them.
