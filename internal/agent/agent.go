// Package session runs a tool inside a PTY and relays bytes between it and the
// user's terminal, passing them through an Interceptor.
package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/thesahibnanda-max/relay/internal/eventlog"
	"github.com/thesahibnanda-max/relay/internal/intercept"
	"github.com/thesahibnanda-max/relay/internal/state"
	relayterm "github.com/thesahibnanda-max/relay/internal/term"
)

// tool is the platform pty/process backend Run drives: creack/pty wrapping an
// *exec.Cmd on Unix (tool_unix.go, behavior-preserving extraction of what
// used to be inline here), a real ConPTY via aymanbagabas/go-pty on Windows
// (tool_windows.go). Everything else in this file is common: only startTool,
// winsize and the tool interface itself are platform-specific.
type tool interface {
	io.Reader
	io.Writer
	io.Closer
	// Resize tells the pty (and so the wrapped process) its terminal is now cols x rows.
	Resize(cols, rows int) error
	// Wait blocks until the process exits.
	Wait() error
	// Signal forwards a signal aimed at Relay itself to the wrapped process.
	// Never called for user-typed Ctrl+C: the pty/ConPTY line discipline
	// already turns that into the child's own SIGINT/CTRL+C by construction.
	Signal(os.Signal) error
	// ExitCode reports the process's exit status once Wait has returned;
	// waitErr is Wait's own return value, for the "never started" case.
	ExitCode(waitErr error) int
}

// drainTimeout bounds how long we wait for trailing output after the child
// exits; a lingering grandchild holding the PTY open would otherwise hang us.
const drainTimeout = 250 * time.Millisecond

type Config struct {
	Tool string
	Bin  string
	Args []string
	Env  []string

	Interceptor intercept.Interceptor
	Log         *eventlog.Logger // may be nil

	// Defaults to os.Stdin / os.Stdout.
	In  *os.File
	Out *os.File

	// RecordInject logs injected text to Log (off when the user asked not to
	// record terminal bytes).
	RecordInject bool

	// ScreenRules lets an adaptor recognise dialogs etc. from screen text.
	ScreenRules []state.ScreenRule
	// OnStart, if set, is called once the tool is running, with a handle for
	// injecting input and querying state. It must not block.
	OnStart func(*Handle)
}

// Handle is the control surface of a running agent.
type Handle struct {
	mux     *InputMux
	tracker *relayterm.Tracker
	machine *state.Machine
	rules   []state.ScreenRule
	out     *os.File
	outMu   *sync.Mutex // serialises every write to the user's terminal
}

// DraftDirty reports whether the user probably has unsent text in the tool's input box.
func (h *Handle) DraftDirty() bool { return h.mux.DraftDirty() }

// ClearDraft forgets the draft estimate.
func (h *Handle) ClearDraft() { h.mux.ClearDraft() }

// Interrupt presses Esc in the tool (at a safe boundary in the user's input).
func (h *Handle) Interrupt(ctx context.Context) error { return h.mux.Raw(ctx, []byte{0x1b}) }

// SetChord reserves a command prefix key in the user's input (see InputMux.SetChord).
func (h *Handle) SetChord(prefix byte, fn func(key byte)) { h.mux.SetChord(prefix, fn) }

// Bell rings the user's terminal bell without interleaving with tool output.
func (h *Handle) Bell() {
	h.outMu.Lock()
	_, _ = h.out.Write([]byte{0x07})
	h.outMu.Unlock()
}

// Inject types text into the tool at the next safe moment (see InputMux).
func (h *Handle) Inject(ctx context.Context, text string, opt InjectOptions) error {
	return h.mux.Inject(ctx, text, opt)
}

// UserQuietFor reports whether the user has been idle at the keyboard for d.
func (h *Handle) UserQuietFor(d time.Duration) bool { return h.mux.UserQuietFor(d) }

// Signal records an authoritative (or heuristic) opinion about the tool's
// state from outside the terminal: a hook, a transcript event.
func (h *Handle) Signal(sig state.Signal) { h.machine.Apply(sig) }

// ClearSignal forgets a source's opinion.
func (h *Handle) ClearSignal(source string) { h.machine.Clear(source) }

// Screen returns the tool's visible screen (after processing all output so far).
func (h *Handle) Screen() []string { h.tracker.Sync(); return h.tracker.Screen() }

// Snapshot returns the merged state of the tool right now.
func (h *Handle) Snapshot() state.Snapshot {
	h.tracker.Sync()
	m := h.tracker.Modes()
	plan := false
	if len(h.rules) > 0 && !h.tracker.Desynced() {
		if sig, p, ok := state.FromScreen(h.tracker.Screen(), h.rules); ok {
			h.machine.Apply(sig)
			plan = p
		} else {
			h.machine.Clear("screen")
		}
	}
	h.machine.SetFlags(plan, m.BracketedPaste, m.AltScreen, h.tracker.Desynced())
	return h.machine.Snapshot()
}

// Run starts the tool and blocks until it exits, returning its exit code.
// The terminal is restored before Run returns.
func Run(cfg Config) (int, error) {
	if cfg.In == nil {
		cfg.In = os.Stdin
	}
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.Interceptor == nil {
		cfg.Interceptor = intercept.Chain(nil)
	}

	isTTY := term.IsTerminal(int(cfg.In.Fd()))

	// Raw mode BEFORE the tool starts: otherwise keys typed during startup are
	// echoed and line-translated (e.g. Enter becomes \n) by the cooked terminal.
	// Restored on every return path of Run; main must not os.Exit before Run returns.
	if isTTY {
		old, err := term.MakeRaw(int(cfg.In.Fd()))
		if err != nil {
			return 1, err
		}
		defer func() { _ = term.Restore(int(cfg.In.Fd()), old) }()
	}

	// Start at the real size so the tool never renders at 80x24 first. Queried
	// on Out, not In: on Windows, GetConsoleScreenBufferInfo (what winsize uses
	// there) requires an output/screen-buffer handle and fails on an input one
	// (confirmed live) - In and Out are the same controlling tty on Unix, so
	// this is a no-op there.
	cols, rows := 80, 24
	if isTTY {
		if c, r, ok := winsize(cfg.Out); ok {
			cols, rows = c, r
		}
	}
	t, err := startTool(cfg.Bin, cfg.Args, cfg.Env, isTTY, cols, rows)
	if err != nil {
		return 1, err
	}
	defer t.Close()

	var outMu sync.Mutex
	tracker := relayterm.New(cols, rows)
	defer tracker.Close()
	machine := state.NewMachine()
	mux := NewInputMux(t)
	if cfg.RecordInject {
		mux.OnInject = func(p []byte) { cfg.Log.Data("inject", p) }
	}
	if cfg.OnStart != nil {
		cfg.OnStart(&Handle{mux: mux, tracker: tracker, machine: machine, rules: cfg.ScreenRules, out: cfg.Out, outMu: &outMu})
	}

	cwd, _ := os.Getwd()
	cfg.Log.Log(eventlog.Event{Type: "start", Tool: cfg.Tool, Bin: cfg.Bin, Args: cfg.Args, Cwd: cwd})

	if isTTY {
		// lastCols/lastRows are only ever touched by whichever ONE of the two
		// goroutines below is actually active on this platform (resizeSignals
		// and pollInterval are complementary: exactly one is non-empty/nonzero
		// per platform), so this is safe without a mutex despite being read
		// and written from a goroutine.
		lastCols, lastRows := cols, rows
		resize := func() {
			if c, r, ok := winsize(cfg.Out); ok {
				_ = t.Resize(c, r)
				lastCols, lastRows = c, r
				tracker.Resize(c, r)
				cfg.Log.Log(eventlog.Event{Type: "resize", Rows: uint16(r), Cols: uint16(c)})
			}
		}
		winch := make(chan os.Signal, 1)
		if sigs := resizeSignals(); len(sigs) > 0 { // (Notify with no signals would subscribe to all of them)
			signal.Notify(winch, sigs...)
		}
		defer signal.Stop(winch)
		go func() {
			for range winch {
				resize()
			}
		}()
		// Windows has no resize signal at all: poll instead. pollInterval is 0
		// on every platform that does have one, so this loop never starts there.
		if iv := pollInterval(); iv > 0 {
			stopPoll := make(chan struct{})
			defer close(stopPoll)
			go func() {
				tk := time.NewTicker(iv)
				defer tk.Stop()
				for {
					select {
					case <-stopPoll:
						return
					case <-tk.C:
						if c, r, ok := winsize(cfg.Out); ok && (c != lastCols || r != lastRows) {
							resize()
						}
					}
				}
			}()
		}
	}

	// Signals aimed at Relay itself (kill, terminal hangup) go to the tool.
	// Ctrl+C typed by the user is not one of these: in raw mode it is byte 0x03
	// which the PTY line discipline turns into SIGINT for the tool.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			_ = t.Signal(s)
		}
	}()

	// tool -> user
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := t.Read(buf)
			if n > 0 {
				if p := cfg.Interceptor.Output(buf[:n]); len(p) > 0 {
					if _, werr := cfg.Out.Write(p); werr != nil {
						return
					}
					// Only after the user has been served: never delay the display.
					tracker.Feed(p)
					machine.Output()
				}
			}
			if err != nil {
				return // EIO once the child side closes; that is normal EOF
			}
		}
	}()

	// user -> tool. Left running when the tool exits; it dies with the process.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := cfg.In.Read(buf)
			if n > 0 {
				if p := cfg.Interceptor.Input(buf[:n]); len(p) > 0 {
					if _, werr := mux.WriteUser(p); werr != nil {
						return
					}
				}
			}
			if err != nil {
				if !isTTY && errors.Is(err, io.EOF) {
					_, _ = mux.WriteUser(eofSignal()) // piped stdin ended
				}
				return
			}
		}
	}()

	waitErr := t.Wait()

	select {
	case <-outDone:
	case <-time.After(drainTimeout):
	}

	return t.ExitCode(waitErr), nil
}
