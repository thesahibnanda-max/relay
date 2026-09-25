package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/thesahibnanda-max/relay/internal/adaptor"
	"github.com/thesahibnanda-max/relay/internal/agent"
	"github.com/thesahibnanda-max/relay/internal/collab"
	"github.com/thesahibnanda-max/relay/internal/ctl"
	"github.com/thesahibnanda-max/relay/internal/daemon"
	"github.com/thesahibnanda-max/relay/internal/eventlog"
	"github.com/thesahibnanda-max/relay/internal/globallink"
	"github.com/thesahibnanda-max/relay/internal/intercept"
	"github.com/thesahibnanda-max/relay/internal/link"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
	"github.com/thesahibnanda-max/relay/internal/roles"
)

func runAgent(p Parsed, factory *adaptor.AdaptorFactory, in io.Reader, errw io.Writer) int {
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
	var lk collab.Link
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

	// lk's nil-check MUST come before any method call: unlike *link.Client,
	// a bare collab.Link interface value panics on a true nil receiver, it
	// doesn't degrade gracefully - see connect()/connectLocal()/
	// connectGlobal() for the discipline that guarantees lk is either a true
	// nil interface or a genuinely connected client, never a typed-nil
	// pointer boxed into the interface.
	if lk != nil {
		if id := lk.Identity(); id.Session.Kind == "shared" {
			who := fmt.Sprintf("you are %s (%s)", id.Agent.Name, id.Agent.Role)
			if id.Resumed {
				who = fmt.Sprintf("welcome back, %s (%s) — resumed", id.Agent.Name, id.Agent.Role)
			}
			fmt.Fprintf(errw, "relay: session %s · %s\nrelay: others join with: relay <claude|codex> [role] --session=%s\n",
				id.Session.ID, who, id.Session.ID)
			// The tool switches the terminal to its own alternate screen right
			// after this, hiding the lines above for good (see issue #41) - a
			// fixed delay would just be guessing how long is long enough, so
			// this waits for an explicit acknowledgment instead. Skipped
			// entirely when stdin isn't a real terminal (a pipe, /dev/null, a
			// script) or $RELAY_SKIP_SESSION_PROMPT is set, so nothing that
			// already pipes into relay starts hanging.
			if shouldPauseForBannerAck(in) {
				waitForBannerAck(in, errw)
			}
		}
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
	if lk != nil {
		lk.Close(code)
	}
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
	}
	return code
}

// sessionBannerAckEnvVar lets an advanced/scripted interactive user skip the
// "press Enter" pause below even on a real terminal.
const sessionBannerAckEnvVar = "RELAY_SKIP_SESSION_PROMPT"

// shouldPauseForBannerAck reports whether runAgent should block for an
// explicit acknowledgment after printing the "others join with" banner: only
// when in is a genuine interactive terminal - never a pipe, /dev/null, or a
// test's plain io.Reader, all of which must never hang - and the escape
// hatch isn't set.
func shouldPauseForBannerAck(in io.Reader) bool {
	if os.Getenv(sessionBannerAckEnvVar) != "" {
		return false
	}
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// waitForBannerAck blocks until the user presses Enter, discarding whatever
// they typed - this is the only chance they get to copy the session/join
// line just printed before the tool takes over the screen (issue #41).
func waitForBannerAck(in io.Reader, out io.Writer) {
	fmt.Fprint(out, "relay: press Enter once you've copied the line above - the tool takes over the screen right after\n")
	_, _ = bufio.NewReader(in).ReadString('\n')
}

// connect is the one place that decides local vs global: everything above
// it (runAgent, prepareLaunch, collab.Session, HandleCtl) only ever sees the
// collab.Link interface and never knows which transport it got.
func connect(paths relayhome.Paths, p Parsed, role roles.Role, a adaptor.Adaptor, col *collab.Session) (collab.Link, error) {
	switch p.SessionKind {
	case SessionKindGlobalNew, SessionKindGlobalJoin:
		return connectGlobal(paths, p, role, a, col)
	default:
		return connectLocal(paths, p, role, a, col)
	}
}

// connectLocal makes sure a daemon is running and registers this agent with
// it - today's exact local-session behavior (--session=NEW_LOCAL, a bare
// ULID, or solo), unchanged.
//
// Whenever --session=<id> --name=<x> names a specific, previously-used
// identity, connectLocal looks for a saved resume token first (see
// identity.go) and tries that before ever falling back to an ordinary fresh
// registration - see connectWithResume for the exact fallback rules.
func connectLocal(paths relayhome.Paths, p Parsed, role roles.Role, a adaptor.Adaptor, col *collab.Session) (collab.Link, error) {
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
	opt := link.Options{
		Paths: paths,
		Hello: proto.Hello{
			Session: p.Session, Name: p.Name, Tool: a.Name(), Role: role.Name, RoleSource: role.Source,
			ApproveInbound: p.ApproveInbound, PID: os.Getpid(), Cwd: cwd, Client: Version,
			CanInterrupt: role.CanInterrupt, CanBroadcast: role.CanBroadcast,
		},
		OnDeliver:    col.Deliver,
		OnNotice:     col.Notice,
		EnsureDaemon: func(ctx context.Context) error { _, err := daemon.Ensure(ctx, paths, exe); return err },
	}

	lk, err := connectWithResume(ctx, paths, p, opt)
	if err != nil {
		return nil, err
	}
	if p.Name != "" {
		id := lk.Identity()
		saveIdentity(paths, id.Session.ID, id.Agent.Name, id.Token, a.Name())
	}
	return lk, nil
}

// connectWithResume is connect's core decision: whenever a saved identity
// for (p.Session, p.Name) exists and --fresh wasn't requested, it presents
// that token first, since the whole point of resume is picking the exact
// same agent identity back up (same messages, same reply chains) rather
// than registering a fresh one under a name that already looks occupied.
//
// Fallback rules (all orthogonal to what --resume/--fresh change, see
// below): a stale token (rejected, or the session it named is simply gone)
// deletes the saved identity and falls back to an ordinary fresh
// registration under the same name - safe, since the existing name-uniqueness
// check still rejects a name someone else legitimately holds now. The old
// process's connection not having visibly dropped yet (CodeAgentLive) is
// retried a few times with a short backoff before giving up, since that is
// often transient rather than a real conflict.
//
// --resume changes only the failure behavior: no saved identity, or the
// resume attempt failing for any reason, is a loud, immediate error instead
// of a silent fallback to fresh registration - for a caller that specifically
// wants "resume this or tell me why not," not "get me an agent somehow."
// --fresh skips the lookup entirely and always registers new, on purpose.
func connectWithResume(ctx context.Context, paths relayhome.Paths, p Parsed, opt link.Options) (*link.Client, error) {
	canResume := p.Name != "" && p.Session != "" && p.Session != proto.SessionNew && !p.Fresh
	if !canResume {
		if p.Resume {
			return nil, fmt.Errorf("--resume needs --session=<id> --name=<x> naming a specific, already-registered agent")
		}
		return link.Connect(ctx, opt)
	}
	saved, ok := loadIdentity(paths, p.Session, p.Name)
	if !ok {
		if p.Resume {
			return nil, fmt.Errorf("no saved identity for %q in session %s (drop --resume to register fresh automatically, or add --fresh to do that on purpose)", p.Name, p.Session)
		}
		return link.Connect(ctx, opt)
	}

	resumeOpt := opt
	resumeOpt.Hello.Token = saved.Token
	lk, err := connectRetryingAgentLive(ctx, resumeOpt)
	if err == nil {
		return lk, nil
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case proto.CodeBadToken, proto.CodeSessionNotFound, proto.CodeSessionEnded:
			deleteIdentity(paths, p.Session, p.Name)
			if p.Resume {
				return nil, err
			}
			return link.Connect(ctx, opt)
		}
	}
	return nil, err
}

// connectRetryingAgentLive retries a resume attempt a few times when the
// daemon reports the old identity as still connected - its process may
// have crashed only moments ago and the daemon hasn't yet noticed the
// connection is gone (it can take up to its own flush-on-close window), a
// transient condition worth a short wait for rather than an immediate,
// confusing failure.
func connectRetryingAgentLive(ctx context.Context, opt link.Options) (*link.Client, error) {
	delays := []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}
	for attempt := 0; ; attempt++ {
		lk, err := link.Connect(ctx, opt)
		if err == nil {
			return lk, nil
		}
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.CodeAgentLive || attempt >= len(delays) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(delays[attempt]):
		}
	}
}

// connectGlobal dials the central server named in p.GlobalToken (joining an
// existing global session) or p.Server/RELAY_SERVER (creating a fresh one
// with --session=NEW), then follows the exact same resume-by-saved-identity
// decision connectLocal makes - see connectGlobalWithResume - just pointed
// at internal/globallink instead of internal/link. No daemon.Ensure/spawn
// logic applies here at all: there is no local process to start.
func connectGlobal(paths relayhome.Paths, p Parsed, role roles.Role, a adaptor.Adaptor, col *collab.Session) (collab.Link, error) {
	hostPort, sessionValue, forceTLS, err := globalDialTarget(p)
	if err != nil {
		return nil, err
	}
	hello := proto.Hello{
		Session: sessionValue, Name: p.Name, Tool: a.Name(), Role: role.Name,
		ApproveInbound: p.ApproveInbound,
		CanInterrupt:   role.CanInterrupt, CanBroadcast: role.CanBroadcast,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	lk, err := connectGlobalWithResume(ctx, paths, p, hostPort, forceTLS, hello, col)
	if err != nil {
		return nil, err
	}
	if p.Name != "" {
		id := lk.Identity()
		saveIdentityWithPolicy(paths, globalIdentityKey(id.Session.ID), id.Agent.Name, id.Token, a.Name(),
			hello.ApproveInbound, hello.CanInterrupt, hello.CanBroadcast)
	}
	return lk, nil
}

// globalDialTarget resolves which server to dial, what Session value to send
// in the Hello, and whether the dial must be forced to TLS. A join token
// already carries the server address and the exact session ULID to resume; a
// fresh session has neither, so it needs --server or RELAY_SERVER to say
// where to create it - there is no implicit default server for a
// local/from-source build (self-hosted infrastructure). An official release
// binary instead has one server baked in at build time - see
// resolveGlobalServer - which both branches below route through, so a
// request for any other host (via --server/RELAY_SERVER or a join token
// naming a different host) is rejected the same way in either case.
func globalDialTarget(p Parsed) (hostPort, sessionValue string, forceTLS bool, err error) {
	if p.SessionKind == SessionKindGlobalJoin {
		hostPort, forceTLS, err := resolveGlobalServer(p.GlobalToken.HostPort)
		if err != nil {
			return "", "", false, err
		}
		return hostPort, p.GlobalToken.ULID, forceTLS, nil
	}
	server := p.Server
	if server == "" {
		server = os.Getenv("RELAY_SERVER")
	}
	hostPort, forceTLS, err = resolveGlobalServer(server)
	if err != nil {
		return "", "", false, err
	}
	if hostPort == "" {
		return "", "", false, errors.New("--session=NEW needs a server to create the session on: pass --server=<host[:port]> or set RELAY_SERVER")
	}
	return hostPort, proto.SessionNew, forceTLS, nil
}

// connectGlobalWithResume mirrors connectWithResume's exact decision shape,
// retargeted at globallink. Resume-by-saved-identity only ever applies to
// SessionKindGlobalJoin (an existing token to resume into) - a brand new
// session (SessionKindGlobalNew) can have no prior saved identity, exactly
// like local's --session=NEW/NEW_LOCAL never resumes either.
func connectGlobalWithResume(ctx context.Context, paths relayhome.Paths, p Parsed, hostPort string, forceTLS bool, hello proto.Hello, col *collab.Session) (*globallink.Client, error) {
	opt := globallink.Options{HostPort: hostPort, ForceTLS: forceTLS, Hello: hello, OnDeliver: col.Deliver, OnNotice: col.Notice}
	canResume := p.Name != "" && p.SessionKind == SessionKindGlobalJoin && !p.Fresh
	if !canResume {
		if p.Resume {
			return nil, fmt.Errorf("--resume needs --session=<token> --name=<x> naming a specific, already-registered agent")
		}
		return globallink.Connect(ctx, opt)
	}
	key := p.GlobalToken.FileSafe()
	saved, ok := loadIdentity(paths, key, p.Name)
	if !ok {
		if p.Resume {
			return nil, fmt.Errorf("no saved identity for %q in session %s (drop --resume to register fresh automatically, or add --fresh to do that on purpose)", p.Name, p.Session)
		}
		return globallink.Connect(ctx, opt)
	}

	resumeOpt := opt
	resumeOpt.Hello.Token = saved.Token
	lk, err := connectGlobalRetryingAgentLive(ctx, resumeOpt)
	if err == nil {
		return lk, nil
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case proto.CodeBadToken, proto.CodeSessionNotFound, proto.CodeSessionEnded:
			deleteIdentity(paths, key, p.Name)
			if p.Resume {
				return nil, err
			}
			return globallink.Connect(ctx, opt)
		}
	}
	return nil, err
}

// connectGlobalRetryingAgentLive mirrors connectRetryingAgentLive exactly,
// retargeted at globallink - the old process may have crashed only moments
// ago and the server hasn't yet noticed the connection is gone.
func connectGlobalRetryingAgentLive(ctx context.Context, opt globallink.Options) (*globallink.Client, error) {
	delays := []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}
	for attempt := 0; ; attempt++ {
		lk, err := globallink.Connect(ctx, opt)
		if err == nil {
			return lk, nil
		}
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.CodeAgentLive || attempt >= len(delays) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(delays[attempt]):
		}
	}
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
		case proto.CodeAgentLive:
			return fmt.Sprintf("%q is still connected elsewhere in that session (wait a moment and try again, or use --fresh to register as a new agent)", p.Name)
		case proto.CodeBadToken:
			return fmt.Sprintf("could not resume %q: its saved identity was rejected (drop --resume to register fresh automatically, or use --fresh)", p.Name)
		}
		return pe.Message
	}
	if errors.Is(err, daemon.ErrVersionMismatch) {
		return err.Error()
	}
	if p.SessionKind == SessionKindGlobalNew || p.SessionKind == SessionKindGlobalJoin {
		return fmt.Sprintf("cannot reach the global session server: %v (use --session=NEW_LOCAL for a local-only session instead)", err)
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
func prepareLaunch(a adaptor.Adaptor, p Parsed, role roles.Role, lk collab.Link, shared bool,
	paths relayhome.Paths, homeOK bool, col *collab.Session, errw io.Writer) (args []string, attach bool, cleanup func()) {
	args, cleanup = p.ToolArgs, func() {}
	if !shared && p.Role == "" {
		return // nothing to add: a plain solo run stays byte-for-byte what the user typed
	}

	// lk can be nil here: e.g. a role without a session (p.Role != "" alone
	// satisfies the guard above even when shared is false) - a true nil
	// collab.Link interface panics on Identity() unlike *link.Client's old
	// nil-receiver safety, so this check is required, not defensive fluff.
	var id proto.Welcome
	if lk != nil {
		id = lk.Identity()
	}
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
