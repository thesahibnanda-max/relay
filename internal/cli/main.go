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
  relay <claude|codex> [role] [--session=NEW|<id>] [--name=<name>] [--approve-inbound]
                       [--record=raw|events|off] [-- <tool arguments>]

  relay claude orchestrator --session=NEW -- --model sonnet   start a new session
  relay codex qa --session=<id> --name=checker                join it as another agent
  relay claude                                                 just log this one, alone

  role     orchestrator, planner, developer, qa, reviewer, or a path to a .md file
  Everything after -- goes to the tool unchanged.

Inspect and manage:
  relay ls [--all] [--session=<id>]      sessions and their agents
  relay session new [--name=<label>]     create a session and print its id
  relay session end <id>                 stop new agents joining a session
  relay daemon [status|stop]             the background service (starts on demand)
  relay send <agent> <text> [--session=<id>] [--priority=low|normal|high|interrupt]
                                         message an agent yourself (text "-" reads stdin)
  relay approve [ls|accept|reject] [<message-id>|all]
                                         release or refuse messages held for agents that
                                         run with --approve-inbound (in that agent's own
                                         terminal: Ctrl+\ then a = approve oldest, r = reject)
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
		return runAgent(p, &factory, errw)
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
