package eventlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readEvents(t *testing.T, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var evs []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", sc.Text(), err)
		}
		evs = append(evs, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	return evs
}

func TestWritesValidJSONLWithPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	l, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}

	buf := []byte("\x1b[Z") // Shift+Tab
	l.Data("in", buf)
	buf[0] = 'X' // caller reuses its buffer; the log must have copied
	l.Log(Event{Type: "resize", Rows: 24, Cols: 80})
	l.Close(3)

	evs := readEvents(t, path)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	if !bytes.Equal(evs[0].Bytes, []byte("\x1b[Z")) {
		t.Errorf("payload not copied: %q", evs[0].Bytes)
	}
	if evs[1].Rows != 24 || evs[1].Cols != 80 {
		t.Errorf("resize: %+v", evs[1])
	}
	if evs[2].Type != "exit" || evs[2].Code == nil || *evs[2].Code != 3 {
		t.Errorf("exit: %+v", evs[2])
	}

	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestNilLoggerIsNoop(t *testing.T) {
	var l *Logger
	l.Data("in", []byte("x"))
	l.Log(Event{Type: "start"})
	l.Close(0)
	if l.Path() != "" {
		t.Error("nil logger path should be empty")
	}
}

func TestDropsInsteadOfBlocking(t *testing.T) {
	// A logger whose writer is never started: the queue fills and Log must drop.
	l := &Logger{ch: make(chan Event, 2), done: make(chan struct{})}
	for i := 0; i < 10; i++ {
		l.Log(Event{Type: "out"})
	}
	if got := l.dropped.Load(); got != 8 {
		t.Errorf("dropped = %d, want 8", got)
	}
}

func TestTeeSeesEveryEventIncludingExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	l, _ := OpenFile(path)
	var got []string
	l.SetTee(func(e Event) { got = append(got, e.Type) })
	buf := []byte("abc")
	l.Data("in", buf)
	buf[0] = 'X'
	l.Log(Event{Type: "resize", Rows: 1, Cols: 2})
	l.Close(0)
	if len(got) != 3 || got[0] != "in" || got[1] != "resize" || got[2] != "exit" {
		t.Fatalf("tee saw %v", got)
	}
	var nilLogger *Logger
	nilLogger.SetTee(func(Event) {}) // must not panic
}
