package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thesahibnanda-max/relay/internal/adaptor"
	"github.com/thesahibnanda-max/relay/internal/agent"
	"github.com/thesahibnanda-max/relay/internal/collab"
	"github.com/thesahibnanda-max/relay/internal/ctl"
	"github.com/thesahibnanda-max/relay/internal/daemon"
	"github.com/thesahibnanda-max/relay/internal/eventlog"
	"github.com/thesahibnanda-max/relay/internal/intercept"
	"github.com/thesahibnanda-max/relay/internal/link"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
	"github.com/thesahibnanda-max/relay/internal/roles"
)

func runAgent(p Parsed, factory *adaptor.AdaptorFactory, errw io.Writer) int {
	a, _ := factory.ByName(p.Tool)

	bin, err := adaptor.ResolveBinary(a.Binary())
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 127
	}

	// Already inside a Relay session (e.g. the tool launched another shimmed
	// tool): run it natively instead of stacking a second layer.
	if os.Getenv(adaptor.EnvActive) != "" {
		err := execReplace(bin, append([]string{a.Binary()}, p.ToolArgs...), os.Environ())
		fmt.Fprintf(errw, "relay: exec %s: %v\n", bin, err)
		return 126
	}

	role, err := roles.Resolve(p.Role)
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 2
	}

	paths, perr := relayhome.Resolve()
	if perr == nil {
		perr = paths.Ensure()
	}
	explicit := p.Session != "" // the user asked for a specific session: never silently go solo

	col := collab.New()
	var lk *link.Client
	if perr == nil {
		lk, err = connect(paths, p, role, a, col)
	} else {
		err = perr
	}
	if err != nil {
		if explicit {
			fmt.Fprintf(errw, "relay: %s\n", friendly(err, p))
			return 1
		}
		lk = nil // solo: carry on without the daemon; the tool must always run
	}

	if id := lk.Identity(); lk != nil && id.Session.Kind == "shared" {
		fmt.Fprintf(errw, "relay: session %s · you are %s (%s)\nrelay: others join with: relay <claude|codex> [role] --session=%s\n",
			id.Session.ID, id.Agent.Name, id.Agent.Role, id.Session.ID)
	}

	var lg *eventlog.Logger
	if p.Record != RecordOff && perr == nil {
		lg, _ = eventlog.Open(paths.SessionsDir(), a.Name()) // a missing log must never stop the tool
	}
	if lk != nil && lg != nil {
		lg.SetTee(func(e eventlog.Event) {
			if ev, ok := link.FromEventlog(e); ok {
				lk.Send(ev)
			}
		})
	}

	chain := intercept.Chain{}
	if p.Record == RecordRaw {
		chain = append(chain, intercept.Log{L: lg})
	}

	env := adaptor.ChildEnv(a)
	shared := false
	if lk != nil {
		id := lk.Identity()
		env = append(env, "RELAY_SESSION_ID="+id.Session.ID, "RELAY_AGENT_ID="+id.Agent.ID, "RELAY_AGENT_NAME="+id.Agent.Name, "RELAY_ROLE="+id.Agent.Role)
		shared = id.Session.Kind == "shared"
	}
	col.Bind(lk, a.Name(), role.Name)
	col.SetUploadTurns(p.Record != RecordOff)

	toolArgs, attach, cleanup := prepareLaunch(a, p, role, lk, shared, paths, perr == nil, col, errw)
	defer cleanup()

	code, err := agent.Run(agent.Config{
		Tool: a.Name(), Bin: bin, Args: toolArgs, Env: env,
		Interceptor: chain, Log: lg, RecordInject: p.Record == RecordRaw,
		ScreenRules: a.ScreenRules(),
		OnStart: func(h *agent.Handle) {
			if attach {
				col.Attach(h, p.ApproveInbound)
			}
		},
	})
	col.Stop()
	cleanup()      // remove the per-launch files before we say goodbye
	lg.Close(code) // also emits the exit event to the daemon via the tee
	lk.Close(code)
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
	}
	return code
}

// connect makes sure a daemon is running and registers this agent with it.
func connect(paths relayhome.Paths, p Parsed, role roles.Role, a adaptor.Adaptor, col *collab.Session) (*link.Client, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := daemon.Ensure(ctx, paths, exe); err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	return link.Connect(ctx, link.Options{
		Paths: paths,
		Hello: proto.Hello{
			Session: p.Session, Name: p.Name, Tool: a.Name(), Role: role.Name, RoleSource: role.Source,
			ApproveInbound: p.ApproveInbound, PID: os.Getpid(), Cwd: cwd, Client: Version,
			CanInterrupt: role.CanInterrupt, CanBroadcast: role.CanBroadcast,
		},
		OnDeliver:    col.Deliver,
		OnNotice:     col.Notice,
		EnsureDaemon: func(ctx context.Context) error { _, err := daemon.Ensure(ctx, paths, exe); return err },
	})
}

// friendly turns connection failures into something a user can act on.
func friendly(err error, p Parsed) string {
	var pe *proto.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case proto.CodeSessionNotFound:
			return fmt.Sprintf("no such session %s (see `relay ls`, or start one with --session=NEW)", p.Session)
		case proto.CodeSessionEnded:
			return fmt.Sprintf("session %s has ended (start a new one with --session=NEW)", p.Session)
		case proto.CodeSessionFull:
			return pe.Message + " (start another session with --session=NEW)"
		case proto.CodeNameTaken:
			return fmt.Sprintf("the name %q is already used in that session (choose another with --name, or omit it to get one automatically)", p.Name)
		}
		return pe.Message
	}
	if errors.Is(err, daemon.ErrVersionMismatch) {
		return err.Error()
	}
	hint := "~/.relay/log/relayd.log"
	if paths, perr := relayhome.Resolve(); perr == nil {
		hint = paths.DaemonLog()
	}
	return fmt.Sprintf("cannot reach the relay daemon: %v (details: %s)", err, hint)
}

// prepareLaunch builds the tool's argv for THIS launch and starts what it
// needs: the control socket for the MCP shim and the ephemeral files in the
// agent's run directory. It never fails the launch: on any problem the tool
// simply runs as the user typed it. The returned cleanup is idempotent and
// removes everything created here.
func prepareLaunch(a adaptor.Adaptor, p Parsed, role roles.Role, lk *link.Client, shared bool,
	paths relayhome.Paths, homeOK bool, col *collab.Session, errw io.Writer) (args []string, attach bool, cleanup func()) {
	args, cleanup = p.ToolArgs, func() {}
	if !shared && p.Role == "" {
		return // nothing to add: a plain solo run stays byte-for-byte what the user typed
	}

	id := lk.Identity()
	spec := adaptor.LaunchSpec{AgentName: id.Agent.Name, Session: id.Session.ID, WithMCP: shared, WithHooks: shared, UserArgs: p.ToolArgs}
	spec.Briefing = collab.Briefing(id, role, shared)

	var once sync.Once
	var runDir string
	var srv *ctl.Server
	cleanup = func() {
		once.Do(func() {
			if srv != nil {
				srv.Close()
			}
			if runDir != "" {
				_ = os.RemoveAll(runDir)
			}
		})
	}
	fallback := func(format string, a ...any) ([]string, bool, func()) {
		fmt.Fprintf(errw, "relay: "+format+" (running the tool without relay features)\n", a...)
		cleanup()
		return p.ToolArgs, false, func() {}
	}

	if shared {
		if !homeOK || lk == nil {
			return fallback("no relay home")
		}
		var err error
		if runDir, err = paths.CreateAgentDir(id.Agent.ID); err != nil {
			return fallback("cannot create run directory: %v", err)
		}
		exe, err := os.Executable()
		if err != nil {
			return fallback("cannot locate the relay binary: %v", err)
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		spec.RunDir, spec.RelayExe = runDir, exe
		if srv, err = ctl.Serve(runDir, col.HandleCtl); err != nil {
			return fallback("cannot open the control socket: %v", err)
		}
	}

	plan, err := a.Prepare(spec)
	if err != nil {
		return fallback("cannot prepare %s: %v", a.Name(), err)
	}
	for _, n := range plan.Notes {
		if !plan.Passthrough {
			fmt.Fprintf(errw, "relay: note: %s\n", n)
		}
	}
	if plan.Passthrough {
		cleanup()
		return plan.Args, false, func() {}
	}
	if spec.Briefing != "" && !plan.BriefingDelivered {
		col.Bus().AddBootstrap(strings.TrimSpace(spec.Briefing))
		attach = true
	}
	if shared {
		attach = true
	}
	return plan.Args, attach, cleanup
}
