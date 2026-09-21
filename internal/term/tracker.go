// Package term keeps a headless copy of the tool's screen so Relay can answer
// "what mode is the terminal in?" and "what is on screen?" without ever
// touching the bytes the user sees. It runs entirely off the display path.
package term

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

const queueSize = 2048 // chunks; ~64 MB worst case at 32 KB each

// Modes are the terminal modes the tool has switched on.
type Modes struct {
	BracketedPaste bool // ?2004
	AltScreen      bool // ?1049 / ?1047
	FocusEvents    bool // ?1004
	Mouse          bool // any of ?1000 ?1002 ?1003
}

// item is either output bytes or a barrier (Sync) marker.
type item struct {
	data    []byte
	barrier chan struct{}
}

// Tracker feeds tool output into a virtual terminal on its own goroutine.
type Tracker struct {
	in   chan item
	quit chan struct{}
	done chan struct{}
	once sync.Once

	mu    sync.Mutex // guards em (not goroutine-safe) and modes
	em    *vt.Emulator
	modes Modes

	desynced atomic.Bool
	dropped  atomic.Uint64
}

// New creates a tracker with the given terminal size.
func New(cols, rows int) *Tracker {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	t := &Tracker{in: make(chan item, queueSize), quit: make(chan struct{}), done: make(chan struct{}), em: vt.NewEmulator(cols, rows)}
	t.em.SetCallbacks(vt.Callbacks{
		EnableMode:  func(m ansi.Mode) { t.setMode(m, true) },
		DisableMode: func(m ansi.Mode) { t.setMode(m, false) },
		AltScreen: func(on bool) {
			t.modes.AltScreen = on // called under t.mu (inside em.Write)
		},
	})
	// The emulator answers terminal queries (DA, DSR, OSC colours...) by
	// writing to an internal pipe that blocks until read. Drain and DISCARD:
	// the user's real terminal already answers those queries, and forwarding
	// ours to the tool would make it see every reply twice.
	go func() { _, _ = io.Copy(io.Discard, t.em) }()
	go t.run()
	return t
}

// setMode runs inside em.Write, so t.mu is already held.
func (t *Tracker) setMode(m ansi.Mode, on bool) {
	switch m {
	case ansi.ModeBracketedPaste:
		t.modes.BracketedPaste = on
	case ansi.ModeFocusEvent:
		t.modes.FocusEvents = on
	case ansi.ModeMouseNormal, ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent:
		t.modes.Mouse = on
	}
}

// Feed queues output bytes for the emulator. It never blocks and copies p.
// If the emulator has fallen behind, the chunk is dropped and the tracker
// reports Desynced until the next full-screen clear lets it recover.
func (t *Tracker) Feed(p []byte) {
	if len(p) == 0 {
		return
	}
	cp := append([]byte(nil), p...)
	select {
	case t.in <- item{data: cp}:
	default:
		t.dropped.Add(1)
		t.desynced.Store(true)
	}
}

func (t *Tracker) run() {
	defer close(t.done)
	for {
		select {
		case <-t.quit:
			return
		case it := <-t.in:
			if it.barrier != nil {
				close(it.barrier)
				continue
			}
			t.mu.Lock()
			_, _ = t.em.Write(it.data)
			t.mu.Unlock()
			if t.desynced.Load() && isFullRedraw(it.data) {
				t.desynced.Store(false)
			}
		}
	}
}

// isFullRedraw spots sequences after which the screen no longer depends on
// anything we may have dropped: erase-display (2J/3J), reset (ESC c), or
// entering the alternate screen.
func isFullRedraw(p []byte) bool {
	return bytes.Contains(p, []byte("\x1b[2J")) || bytes.Contains(p, []byte("\x1b[3J")) ||
		bytes.Contains(p, []byte("\x1bc")) || bytes.Contains(p, []byte("\x1b[?1049h"))
}

// Resize must be called when the real terminal is resized.
func (t *Tracker) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	t.mu.Lock()
	t.em.Resize(cols, rows)
	t.mu.Unlock()
}

// Modes returns the current terminal modes.
func (t *Tracker) Modes() Modes {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.modes
}

// Desynced is true if output was dropped and the screen may be stale.
func (t *Tracker) Desynced() bool { return t.desynced.Load() }

// Dropped counts chunks dropped because the emulator fell behind.
func (t *Tracker) Dropped() uint64 { return t.dropped.Load() }

// Screen returns the visible screen as text, one string per row, with
// trailing spaces trimmed. It reflects everything fed so far only after Sync.
func (t *Tracker) Screen() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(t.em.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

// Sync blocks until everything fed before the call has been processed.
// Intended for tests and for callers about to inspect the screen; it is not
// for the display path.
func (t *Tracker) Sync() {
	b := make(chan struct{})
	select {
	case t.in <- item{barrier: b}:
	case <-t.quit:
		return
	}
	select {
	case <-b:
	case <-t.quit:
	}
}

// Close stops the tracker. Feed and Sync remain safe to call afterwards.
func (t *Tracker) Close() {
	t.once.Do(func() {
		close(t.quit)
		<-t.done
		// Unblock the reply-draining goroutine by closing the pipe's write
		// end directly. vt.Emulator.Close is deliberately not used: it sets a
		// plain bool that its own Read reads, which the race detector flags.
		if pw, ok := t.em.InputPipe().(*io.PipeWriter); ok {
			_ = pw.CloseWithError(io.EOF)
		}
	})
}
