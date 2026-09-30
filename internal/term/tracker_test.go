package term

import (
	"strings"
	"testing"
	"time"
)

func TestTracksModes(t *testing.T) {
	tr := New(80, 24)
	defer tr.Close()

	tr.Feed([]byte("\x1b[?2004h\x1b[?1004h\x1b[?1003h"))
	tr.Sync()
	m := tr.Modes()
	if !m.BracketedPaste || !m.FocusEvents || !m.Mouse || m.AltScreen {
		t.Fatalf("after enable: %+v", m)
	}

	tr.Feed([]byte("\x1b[?1049h\x1b[?2004l\x1b[?1003l"))
	tr.Sync()
	m = tr.Modes()
	if m.BracketedPaste || m.Mouse || !m.AltScreen || !m.FocusEvents {
		t.Fatalf("after changes: %+v", m)
	}
}

func TestModeSequenceSplitAcrossChunks(t *testing.T) {
	tr := New(80, 24)
	defer tr.Close()
	for _, part := range []string{"\x1b[?20", "04", "h"} {
		tr.Feed([]byte(part))
	}
	tr.Sync()
	if !tr.Modes().BracketedPaste {
		t.Fatal("split sequence not recognised")
	}
}

func TestScreenText(t *testing.T) {
	tr := New(40, 5)
	defer tr.Close()
	tr.Feed([]byte("\x1b[2J\x1b[H\x1b[31mhello\x1b[0m\r\n> draft text"))
	tr.Sync()
	scr := strings.Join(tr.Screen(), "\n")
	if !strings.Contains(scr, "hello") || !strings.Contains(scr, "> draft text") {
		t.Fatalf("screen = %q", scr)
	}
}

// The emulator replies to terminal queries on an internal pipe; if nobody
// drains it, Write blocks forever and the tracker silently stops.
func TestQueryRepliesDoNotBlockTracker(t *testing.T) {
	tr := New(80, 24)
	defer tr.Close()
	var q strings.Builder
	for i := 0; i < 200; i++ {
		q.WriteString("\x1b[c\x1b[6n\x1b[>c\x1b]10;?\x07")
	}
	tr.Feed([]byte(q.String()))
	tr.Feed([]byte("\x1b[?2004h"))

	done := make(chan struct{})
	go func() { tr.Sync(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("tracker blocked on unread query replies")
	}
	if !tr.Modes().BracketedPaste {
		t.Fatal("output after the queries was not processed")
	}
}

func TestResize(t *testing.T) {
	tr := New(80, 24)
	defer tr.Close()
	tr.Resize(100, 30)
	tr.Sync()
	tr.Feed([]byte("x"))
	tr.Sync()
}

func TestDesyncAndRecovery(t *testing.T) {
	tr := &Tracker{in: make(chan item, 1), quit: make(chan struct{}), done: make(chan struct{})}
	// No goroutine draining: the second Feed must drop, not block.
	tr.Feed([]byte("a"))
	tr.Feed([]byte("b"))
	if !tr.Desynced() || tr.Dropped() != 1 {
		t.Fatalf("desynced=%v dropped=%d", tr.Desynced(), tr.Dropped())
	}
	if !isFullRedraw([]byte("junk\x1b[2Jmore")) || isFullRedraw([]byte("plain")) {
		t.Fatal("isFullRedraw misclassified")
	}
}

func TestFeedAfterCloseIsSafe(t *testing.T) {
	tr := New(80, 24)
	tr.Close()
	tr.Feed([]byte("x"))
	tr.Sync()
	tr.Close()
}

// agy's startup and exit sequences (confirmed live): it pushes kitty flags
// and sets modifyOtherKeys, and undoes both on a clean exit.
func TestKeyboardModesAndResetSequence(t *testing.T) {
	tr := New(80, 24)
	defer tr.Close()
	tr.Feed([]byte("\x1b[?2004h\x1b[>1u\x1b[>4;2m\x1b[?25l"))
	tr.Sync()
	m := tr.Modes()
	if m.KittyKeyboard != 1 || !m.ModifyOtherKeys || !m.CursorHidden || !m.BracketedPaste {
		t.Fatalf("modes %+v", m)
	}
	want := "\x1b[<1u\x1b[>4m\x1b[?2004l\x1b[?25h"
	if got := m.ResetSequence(); got != want {
		t.Fatalf("reset %q, want %q", got, want)
	}
	tr.Feed([]byte("\x1b[<1u\x1b[>4m\x1b[?2004l\x1b[?25h")) // agy's own clean exit
	tr.Sync()
	if got := tr.Modes().ResetSequence(); got != "" {
		t.Fatalf("a tool that restored everything must leave nothing to reset, got %q", got)
	}
	tr.Feed([]byte("\x1b[>1u\x1b[>3u\x1b[<5u\x1b[>4;0m"))
	tr.Sync()
	if m := tr.Modes(); m.KittyKeyboard != 0 || m.ModifyOtherKeys {
		t.Fatalf("over-popping and modifyOtherKeys 0: %+v", m)
	}
	tr.Feed([]byte("\x1b[?1049h\x1b[?1000h\x1b[?1004h"))
	tr.Sync()
	if got := tr.Modes().ResetSequence(); got != "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?1049l" {
		t.Fatalf("reset %q", got)
	}
}
