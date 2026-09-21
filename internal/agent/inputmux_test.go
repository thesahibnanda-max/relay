package agent

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func feedAll(p *inputParser, s string) {
	for i := 0; i < len(s); i++ {
		p.feed(s[i])
	}
}

func TestParserSafeBoundaries(t *testing.T) {
	old := 100 * time.Millisecond // past escTimeout, well before staleTimeout
	cases := []struct {
		name     string
		in       string
		safeNow  bool // immediately after the last byte
		safeLate bool // after escTimeout elapsed
	}{
		{"empty", "", true, true},
		{"plain text", "hello", true, true},
		{"enter", "hi\r", true, true},
		{"lone ESC (Esc key or start of sequence)", "\x1b", false, true},
		{"CSI partial", "\x1b[", false, false},
		{"CSI params partial", "\x1b[1;3", false, false},
		{"Shift+Tab complete", "\x1b[Z", true, true},
		{"Alt+Up complete", "\x1b[1;3A", true, true},
		{"Alt+x", "\x1bx", true, true},
		{"SS3 partial", "\x1bO", false, false},
		{"SS3 complete (F1)", "\x1bOP", true, true},
		{"mouse SGR complete", "\x1b[<35;10;20M", true, true},
		{"mouse SGR partial", "\x1b[<35;10", false, false},
		{"OSC unterminated", "\x1b]10;rgb:ffff", false, false},
		{"OSC BEL-terminated", "\x1b]0;title\x07", true, true},
		{"OSC ST-terminated", "\x1b]0;title\x1b\\", true, true},
		{"utf8 2-byte partial", "\xc3", false, false},
		{"utf8 2-byte complete", "\xc3\xa9", true, true},
		{"utf8 4-byte partial", "\xf0\x9f\x98", false, false},
		{"utf8 4-byte complete", "\xf0\x9f\x98\x80", true, true},
		{"paste open", "\x1b[200~line one\nline", false, false},
		{"paste closed", "\x1b[200~text\x1b[201~", true, true},
		{"paste with ESC inside, still open", "\x1b[200~a\x1b[Ab", false, false},
		{"ESC ESC", "\x1b\x1b", false, true},
	}
	for _, c := range cases {
		var p inputParser
		feedAll(&p, c.in)
		if got := p.safe(0); got != c.safeNow {
			t.Errorf("%s: safe(now) = %v, want %v", c.name, got, c.safeNow)
		}
		if got := p.safe(old); got != c.safeLate {
			t.Errorf("%s: safe(late) = %v, want %v", c.name, got, c.safeLate)
		}
	}
}

// Sequences must be recognised even when split across arbitrary chunk
// boundaries, one byte at a time.
func TestParserByteAtATimeMatchesWholeChunk(t *testing.T) {
	seqs := []string{"\x1b[Z", "\x1b[1;3A", "\x1b[<35;10;20M", "\x1b[200~x\ny\x1b[201~", "\x1b]0;t\x07", "é😀"}
	for _, s := range seqs {
		var whole, bytewise inputParser
		feedAll(&whole, s)
		for i := 0; i < len(s); i++ {
			bytewise.feed(s[i])
		}
		if whole.safe(0) != bytewise.safe(0) || !whole.safe(0) {
			t.Errorf("%q: whole=%v bytewise=%v, both should be safe", s, whole.safe(0), bytewise.safe(0))
		}
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestInjectWaitsForSafeBoundary(t *testing.T) {
	out := &lockedBuf{}
	m := NewInputMux(out)

	// User is mid-sequence (typed ESC [ but not the final byte).
	m.WriteUser([]byte("\x1b["))

	done := make(chan error, 1)
	go func() { done <- m.Inject(context.Background(), "hello", InjectOptions{Paste: true, Submit: true}) }()

	select {
	case <-done:
		t.Fatal("inject completed while user was mid-escape-sequence")
	case <-time.After(60 * time.Millisecond):
	}
	if strings.Contains(out.String(), "hello") {
		t.Fatalf("injected bytes leaked into the sequence: %q", out.String())
	}

	m.WriteUser([]byte("Z")) // completes Shift+Tab
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := "\x1b[Z" + "\x1b[200~hello\x1b[201~\r"
	// The user's "\x1b[" + "Z" are contiguous; injection comes strictly after.
	if got := out.String(); got != "\x1b["+"Z"+"\x1b[200~hello\x1b[201~\r" {
		t.Errorf("stream = %q, want %q", got, want)
	}
}

func TestAbandonedSequenceEventuallyStopsBlocking(t *testing.T) {
	var p inputParser
	feedAll(&p, "\x1b[1;3")
	if p.safe(staleTimeout - time.Millisecond) {
		t.Error("safe too early")
	}
	if !p.safe(staleTimeout) {
		t.Error("abandoned CSI must stop blocking after staleTimeout")
	}
	var q inputParser
	feedAll(&q, "\x1b[200~half a paste")
	if q.safe(staleTimeout) || !q.safe(staleTimeoutPaste) {
		t.Error("paste should use the longer stale timeout")
	}
}

func TestInjectHonoursContext(t *testing.T) {
	m := NewInputMux(&lockedBuf{})
	m.WriteUser([]byte("\x1b[")) // stuck mid-sequence forever
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := m.Inject(ctx, "x", InjectOptions{}); err == nil {
		t.Fatal("expected context error")
	}
}

func TestInjectionCannotEscapeThePaste(t *testing.T) {
	got := string(buildInjection("a\x1b[201~\x1b[200~rm -rf /\x07\x7f b\r\nc\rd", InjectOptions{Paste: true}))
	if strings.Count(got, "\x1b") != 2 { // only our own start + end markers
		t.Errorf("payload contains stray ESC: %q", got)
	}
	want := "\x1b[200~a[201~[200~rm -rf / b\nc\nd\x1b[201~"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestInjectWithoutPasteFlattensNewlines(t *testing.T) {
	got := string(buildInjection("line1\nline2", InjectOptions{Submit: true}))
	if got != "line1 line2\r" {
		t.Errorf("got %q", got)
	}
}

func TestUserBytesNeverReordered(t *testing.T) {
	out := &lockedBuf{}
	m := NewInputMux(out)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = m.Inject(context.Background(), "INJ", InjectOptions{})
			time.Sleep(time.Millisecond)
		}
	}()
	want := ""
	for i := 0; i < 200; i++ {
		s := "\x1b[1;3A" // Alt+Up: must never be split by an injection
		want += s
		m.WriteUser([]byte(s))
		time.Sleep(200 * time.Microsecond)
	}
	wg.Wait()
	// Removing the injections must leave the user's stream byte-identical.
	if got := strings.ReplaceAll(out.String(), "INJ", ""); got != want {
		t.Fatal("user byte stream was altered or split by injection")
	}
	if strings.Count(out.String(), "INJ") != 20 {
		t.Errorf("expected 20 injections, got %d", strings.Count(out.String(), "INJ"))
	}
}

func TestDraftTracking(t *testing.T) {
	var buf bytes.Buffer
	m := NewInputMux(&buf)
	now := time.Now()
	m.now = func() time.Time { return now }

	step := func(keys string, want bool, what string) {
		t.Helper()
		m.WriteUser([]byte(keys))
		if got := m.DraftDirty(); got != want {
			t.Fatalf("%s: DraftDirty=%v, want %v", what, got, want)
		}
	}
	step("", false, "fresh")
	step("\x1b[Z", false, "Shift+Tab is not text")
	step("\x1b[D\x1b[C", false, "left/right are not text")
	step("h", true, "a character")
	step("\r", false, "Enter submits")
	step("é", true, "multi-byte character")
	step("\x03", false, "Ctrl+C clears")
	step("\x1b[A", true, "history recall may fill the box")
	step("\r", false, "submit")
	step("\x1b[200~pasted\ntext\x1b[201~", true, "a paste")
	step("\x1bOA", true, "application-mode Up")
	step("\x1b[200~a\rb\x1b[201~\r", false, "Enter after a paste; a CR inside the paste is not a submit")
	step("x", true, "typing again")
	m.ClearDraft()
	if m.DraftDirty() {
		t.Fatal("ClearDraft")
	}
	step("y", true, "typing")
	now = now.Add(draftDecay + time.Second)
	if m.DraftDirty() {
		t.Fatal("an untouched draft is eventually considered abandoned")
	}
	if !bytes.Contains(buf.Bytes(), []byte("pasted")) {
		t.Fatal("bytes must be forwarded unchanged")
	}
}

func TestChordSwallowsCommandKeysOnly(t *testing.T) {
	var buf bytes.Buffer
	m := NewInputMux(&buf)
	now := time.Now()
	m.now = func() time.Time { return now }
	keys := make(chan byte, 8)
	m.SetChord(0x1c, func(k byte) { keys <- k })

	m.WriteUser([]byte("ab\x1cAcd")) // prefix + a: command, in the middle of a chunk
	if got := buf.String(); got != "abcd" {
		t.Fatalf("forwarded %q", got)
	}
	select {
	case k := <-keys:
		if k != 'a' {
			t.Fatalf("key %q", k)
		}
	case <-time.After(time.Second):
		t.Fatal("chord handler not called")
	}

	buf.Reset()
	m.WriteUser([]byte("\x1c"))
	m.WriteUser([]byte("r")) // split across writes
	if buf.Len() != 0 {
		t.Fatalf("nothing forwarded: %q", buf.String())
	}
	if k := <-keys; k != 'r' {
		t.Fatalf("key %q", k)
	}

	buf.Reset()
	m.WriteUser([]byte("\x1c\x1c")) // doubled: literal prefix
	m.WriteUser([]byte("\x1cz"))    // unknown: both pass through
	if got := buf.String(); got != "\x1c\x1cz" {
		t.Fatalf("passthrough: %q", got)
	}
	select {
	case k := <-keys:
		t.Fatalf("unexpected command %q", k)
	case <-time.After(50 * time.Millisecond):
	}

	// an expired prefix is dropped, and the next key is normal input
	buf.Reset()
	m.WriteUser([]byte("\x1c"))
	now = now.Add(chordTimeout + time.Second)
	m.WriteUser([]byte("a"))
	if buf.String() != "a" {
		t.Fatalf("after timeout: %q", buf.String())
	}

	// inside a paste the prefix byte is just data
	buf.Reset()
	m.WriteUser([]byte("\x1b[200~x\x1cay\x1b[201~"))
	if buf.String() != "\x1b[200~x\x1cay\x1b[201~" {
		t.Fatalf("paste must pass untouched: %q", buf.String())
	}
	select {
	case k := <-keys:
		t.Fatalf("chord fired inside a paste: %q", k)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRawWaitsForSafeBoundary(t *testing.T) {
	var buf bytes.Buffer
	m := NewInputMux(&buf)
	m.WriteUser([]byte("\x1b[1")) // user is midway through a CSI sequence
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.Raw(ctx, []byte{0x1b}); err == nil {
		t.Fatal("must not write inside the user's sequence")
	}
	m.WriteUser([]byte(";5A"))
	if err := m.Raw(context.Background(), []byte{0x1b}); err != nil || !bytes.HasSuffix(buf.Bytes(), []byte{0x1b}) {
		t.Fatalf("raw write at a boundary: %v %q", err, buf.String())
	}
}
