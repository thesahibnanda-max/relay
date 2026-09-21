// Package eventlog writes session events as JSONL without ever blocking the
// caller. Relay sits on the hot path of an interactive TUI, so a slow disk must
// never add latency: when the buffer is full, events are dropped and counted.
package eventlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const queueSize = 4096

// Event is one JSONL line.
type Event struct {
	Time    time.Time `json:"t"`
	Type    string    `json:"type"`
	Bytes   []byte    `json:"b64,omitempty"` // encoded as base64 by encoding/json
	Tool    string    `json:"tool,omitempty"`
	Bin     string    `json:"bin,omitempty"`
	Args    []string  `json:"args,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	Rows    uint16    `json:"rows,omitempty"`
	Cols    uint16    `json:"cols,omitempty"`
	Code    *int      `json:"code,omitempty"`
	Dropped uint64    `json:"dropped,omitempty"`
}

// Logger is safe for concurrent use. A nil *Logger is a valid no-op logger, so
// callers can keep passing through if the log file could not be opened.
type Logger struct {
	tee     atomic.Pointer[func(Event)]
	ch      chan Event
	done    chan struct{}
	dropped atomic.Uint64
	once    sync.Once
	path    string
}

// Open creates a session log under dir (typically ~/.relay/sessions).
func Open(dir, tool string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%s-%d.jsonl", time.Now().UTC().Format("20060102T150405Z"), tool, os.Getpid())
	return OpenFile(filepath.Join(dir, name))
}

// OpenFile logs to an explicit path (mode 0600: keystrokes may contain secrets).
func OpenFile(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l := &Logger{ch: make(chan Event, queueSize), done: make(chan struct{}), path: path}
	go l.run(f)
	return l, nil
}

// DefaultDir returns ~/.relay/sessions.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".relay", "sessions"), nil
}

// SetTee makes every logged event also be passed to fn (e.g. to upload it).
// fn is called on the logging goroutine and must not block; the event's Bytes
// are a private copy that fn may keep.
func (l *Logger) SetTee(fn func(Event)) {
	if l == nil {
		return
	}
	l.tee.Store(&fn)
}

// Path returns the log file path ("" for a nil logger).
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *Logger) run(f *os.File) {
	defer close(l.done)
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for ev := range l.ch {
		_ = enc.Encode(ev)
		// Flush when idle so a crash loses at most the in-flight batch.
		if len(l.ch) == 0 {
			_ = w.Flush()
		}
	}
	_ = w.Flush()
}

// Log enqueues an event; it never blocks. Byte payloads are copied because
// callers reuse their read buffers.
func (l *Logger) Log(ev Event) {
	if l == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if ev.Bytes != nil {
		ev.Bytes = append([]byte(nil), ev.Bytes...)
	}
	if fn := l.tee.Load(); fn != nil {
		(*fn)(ev)
	}
	select {
	case l.ch <- ev:
	default:
		l.dropped.Add(1)
	}
}

// Data logs a raw byte event; dir is "in" (user->tool) or "out" (tool->user).
func (l *Logger) Data(dir string, p []byte) {
	l.Log(Event{Type: dir, Bytes: p})
}

// Close writes the final exit event (including the dropped count), flushes and
// stops the writer. The exit event is enqueued blocking so it is never lost.
func (l *Logger) Close(exitCode int) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		ev := Event{Time: time.Now().UTC(), Type: "exit", Code: &exitCode, Dropped: l.dropped.Load()}
		if fn := l.tee.Load(); fn != nil {
			(*fn)(ev)
		}
		l.ch <- ev
		close(l.ch)
		<-l.done
	})
}
