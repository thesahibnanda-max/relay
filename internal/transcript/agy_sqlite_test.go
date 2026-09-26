package transcript

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// openTestStepsDB creates a fresh SQLite file with just the columns of agy's
// real `steps` table (schema confirmed live against a real agy 1.2.11
// conversation database) that SQLiteTailer actually reads.
func openTestStepsDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(3000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER NOT NULL, status INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertStep(t *testing.T, db *sql.DB, idx, stepType, status int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, status) VALUES (?, ?, ?)`, idx, stepType, status); err != nil {
		t.Fatal(err)
	}
}

func updateStepStatus(t *testing.T, db *sql.DB, idx, status int) {
	t.Helper()
	if _, err := db.Exec(`UPDATE steps SET status=? WHERE idx=?`, status, idx); err != nil {
		t.Fatal(err)
	}
}

// collector is a concurrency-safe sink for SQLiteTailer.Fn.
type collector struct {
	mu      sync.Mutex
	signals []string
}

func (c *collector) add(r Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signals = append(c.signals, r.Signal)
}

func (c *collector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.signals...)
}

// TestSQLiteTailerMapsStepSequenceToSignals replays a sequence confirmed live
// against a real agy conversation database (user message, assistant message,
// a tool call, assistant message, user message, assistant message) and
// checks the exact resulting signal sequence: step_type 132 (tool call)
// reaching status 3 must never be mistaken for the turn ending - only
// step_type 15 (assistant message) at status 3 does that.
func TestSQLiteTailerMapsStepSequenceToSignals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	db := openTestStepsDB(t, path)
	for _, r := range []struct{ idx, stepType, status int }{
		{0, agyStepUserMessage, agyStatusComplete},
		{1, agyStepAssistantMessage, agyStatusComplete},
		{2, 132, agyStatusComplete}, // tool call: must not signal task_complete
		{3, agyStepAssistantMessage, agyStatusComplete},
		{4, agyStepUserMessage, agyStatusComplete},
		{5, agyStepAssistantMessage, agyStatusComplete},
	} {
		insertStep(t, db, r.idx, r.stepType, r.status)
	}
	db.Close()

	c := &collector{}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	tl := &SQLiteTailer{Path: path, Fn: c.add, Every: 20 * time.Millisecond}
	tl.Run(ctx)

	want := []string{SigTaskStarted, SigTaskComplete, SigTaskComplete, SigTaskStarted, SigTaskComplete}
	if got := c.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}

// TestSQLiteTailerHandlesInPlaceMutation covers the one thing the line-based
// Tailer cannot do at all: agy's `steps` table mutates a row in place rather
// than appending a new one. An assistant-message step sitting at status=2
// (still running) must fire nothing until it is updated to status=3 - and
// the tailer must notice that update on its next poll, not just on new rows.
func TestSQLiteTailerHandlesInPlaceMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	db := openTestStepsDB(t, path)
	insertStep(t, db, 0, agyStepAssistantMessage, 2)
	db.Close()

	c := &collector{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tl := &SQLiteTailer{Path: path, Fn: c.add, Every: 20 * time.Millisecond}
	go tl.Run(ctx)

	time.Sleep(80 * time.Millisecond) // several polls at status=2: nothing should fire
	if got := c.snapshot(); len(got) != 0 {
		t.Fatalf("signals before the update = %v, want none", got)
	}

	db2, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(3000)")
	if err != nil {
		t.Fatal(err)
	}
	updateStepStatus(t, db2, 0, agyStatusComplete)
	db2.Close()

	deadline := time.Now().Add(2 * time.Second)
	for len(c.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := c.snapshot(); !reflect.DeepEqual(got, []string{SigTaskComplete}) {
		t.Fatalf("signals after the update = %v, want [task_complete]", got)
	}
}

func TestSQLiteTailerMissingFileIsANoOp(t *testing.T) {
	c := &collector{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	tl := &SQLiteTailer{Path: filepath.Join(t.TempDir(), "missing.db"), Fn: c.add, Every: 20 * time.Millisecond}
	tl.Run(ctx) // must not panic or block past the deadline
	if got := c.snapshot(); len(got) != 0 {
		t.Fatalf("signals = %v, want none", got)
	}
}
