package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/thesahibnanda-max/relay/internal/adaptor"
)

const usageText = `relay: a transparent layer between you and your AI coding agents.

Run an agent (it looks and behaves exactly like the tool itself):
  relay <claude|codex> [role] [--session=NEW|NEW_LOCAL|<id>|<id>@host[:port]] [--name=<name>]
                       [--server=<host[:port]>] [--resume | --fresh] [--approve-inbound]
                       [--record=raw|events|off] [-- <tool arguments>]

  relay claude orchestrator --session=NEW --server=host:5555 -- --model sonnet
                                                                start a new GLOBAL session (any machine can
                                                                join) - source builds only; the official
                                                                binary needs no --server (see below)
  relay codex qa --session=<ulid>@host:5555 --name=checker    join that global session from another machine
                                                                - source builds only (see below for the
                                                                official binary)
  relay claude orchestrator --session=NEW_LOCAL -- --model sonnet   start a LOCAL-only session (this machine only)
  relay codex qa --session=<id> --name=checker                join a local session as another agent
  relay claude                                                 just log this one, alone

  role     orchestrator, planner, developer, qa, reviewer, or a path to a .md file
  Everything after -- goes to the tool unchanged.
  --session=NEW creates a new global session (dials a central server named by --server or
                       $RELAY_SERVER); --session=NEW_LOCAL creates a local-only session on this
                       machine, same as always. A session token printed by "others join with"
                       (a bare ULID for local, "<ulid>@host:port" for global) is what --session
                       takes to join one.
                       The officially distributed relay binary has one such server built in and
                       needs none of the above: --server/$RELAY_SERVER/a join token's host are
                       ignored when they already name that one server, and rejected with a clear
                       error otherwise. Only a binary built from source (e.g. "make build") can
                       point them at a different server.
  --session=<id> --name=<name> auto-resumes a crashed/killed agent of that name if
                       a saved identity exists (nothing to opt into); --resume fails
                       loudly instead of silently registering fresh when none is found,
                       --fresh ignores any saved identity and always registers new

Inspect and manage:
  relay ls [--all] [--session=<id>]      sessions and their agents
  relay session new [--name=<label>]     create a session and print its id
  relay session end <id>                 stop new agents joining a session
  relay daemon [status|stop]             the background service (starts on demand)
  relay send <agent> <text> [--session=<id>] [--priority=low|normal|high|interrupt]
                                         message an agent yourself (text "-" reads stdin)
  relay approve [ls|accept|reject] [<message-id>|all] [--session=<id> --name=<agent>]
                                         release or refuse messages held for agents that
                                         run with --approve-inbound (in that agent's own
                                         terminal: Ctrl+\ then a = approve oldest, r = reject);
                                         --session=<global token> --name=<agent> acts on one
                                         named agent's own held mail in a global session
  relay messages [--session=<id>] [--agent=<name>] [--state=<s>] [--limit=<n>]
                                         what agents said to each other
  relay gc [--older-than=30d] [--compress] [--dry-run]
                                         remove leftovers of crashed agents; optionally forget
                                         sessions idle longer than that and zstd-compress closed logs
  relay version

Files live in ~/.relay (override with RELAY_HOME).
`

// Main runs relay with the given argv (including argv[0]) and returns the exit status.
func Main(argv []string) int {
	factory := adaptor.NewAdaptorFactory()
	p, err := Parse(argv, &factory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\nTry `relay help`.\n", err)
		var ue *UsageError
		if errors.As(err, &ue) {
			return 2
		}
		return 1
	}
	out, errw := io.Writer(os.Stdout), io.Writer(os.Stderr)
	if !supported && p.Kind != KindHelp && p.Kind != KindVersion {
		fmt.Fprintln(errw, "relay: this platform is not supported. Relay runs on Linux, macOS and WSL (Windows Subsystem for Linux): install and run it inside WSL.")
		return 1
	}
	switch p.Kind {
	case KindHelp:
		fmt.Fprint(out, usageText)
		return 0
	case KindVersion:
		fmt.Fprintln(out, "relay", Version)
		return 0
	case KindAgent:
		return runAgent(p, &factory, os.Stdin, errw)
	case KindLs:
		return runLs(p, out, errw)
	case KindSessionNew, KindSessionEnd:
		return runSession(p, out, errw)
	case KindDaemon, KindDaemonStop, KindDaemonStatus:
		return runDaemon(p, out, errw)
	case KindMCP:
		return runMCP(p)
	case KindDoctor:
		return runDoctor(out, errw)
	case KindHook:
		return runHook(p, os.Stdin, out)
	case KindApprove:
		return runApprove(p, os.Stdin, out, errw)
	case KindSend:
		return runSend(p, os.Stdin, out, errw)
	case KindMessages:
		return runMessages(p, out, errw)
	case KindGC:
		return runGC(p, out, errw)
	}
	return 2
}
