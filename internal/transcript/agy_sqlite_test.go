package transcript

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// agy's real schema (1.2.12/1.2.13), trimmed to the tables relay reads.
const agySchema = "CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0,`status` integer NOT NULL DEFAULT 0,`has_subtrajectory` numeric NOT NULL DEFAULT false,`metadata` blob,`error_details` blob,`permissions` blob,`task_details` blob,`render_info` blob,`step_payload` blob,`step_format` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`));" +
	"CREATE TABLE `executor_metadata` (`idx` integer,`data` blob,PRIMARY KEY (`idx`));" +
	"CREATE TABLE `trajectory_metadata_blob` (`id` text DEFAULT \"main\",`data` blob,PRIMARY KEY (`id`));"

// pb builds protobuf wire bytes: pbVar(field, n) and pbLen(field, bytes...).
func pbVar(field int, n uint64) []byte {
	return append(pbKey(field, 0), pbUvarint(n)...)
}

func pbLen(field int, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	out := append(pbKey(field, 2), pbUvarint(uint64(len(body)))...)
	return append(out, body...)
}

func pbKey(field, wire int) []byte { return pbUvarint(uint64(field<<3 | wire)) }

func pbUvarint(n uint64) []byte {
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	return append(out, byte(n))
}

type fakeConv struct {
	t  *testing.T
	db *sql.DB
}

func newConv(t *testing.T, path string, wal bool) *fakeConv {
	t.Helper()
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p}).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if wal {
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(agySchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &fakeConv{t: t, db: db}
}

func (c *fakeConv) step(idx, typ, status int, payload []byte) {
	c.t.Helper()
	if _, err := c.db.Exec("INSERT OR REPLACE INTO steps(idx, step_type, status, step_payload) VALUES(?,?,?,?)", idx, typ, status, payload); err != nil {
		c.t.Fatal(err)
	}
}

func (c *fakeConv) status(idx, status int) {
	c.t.Helper()
	if _, err := c.db.Exec("UPDATE steps SET status=? WHERE idx=?", status, idx); err != nil {
		c.t.Fatal(err)
	}
}

// endTurn writes agy's end-of-turn row: field 1 outcome (4 done, 2 cancelled), field 3 last idx.
func (c *fakeConv) endTurn(n, outcome, lastIdx int) {
	c.t.Helper()
	data := append(append(pbVar(1, uint64(outcome)), pbVar(2, 1)...), pbVar(3, uint64(lastIdx))...)
	if _, err := c.db.Exec("INSERT INTO executor_metadata(idx, data) VALUES(?,?)", n, data); err != nil {
		c.t.Fatal(err)
	}
}

func userPayload(text string) []byte {
	return append(pbLen(5, pbLen(1, pbVar(1, 1790000000), pbVar(2, 5))), pbLen(19, pbLen(2, []byte(text)))...)
}

func answerPayload(text string) []byte {
	return pbLen(20, pbLen(1, []byte(text)), pbLen(3, []byte("thinking")))
}

func toolPlanPayload() []byte { return pbLen(20, pbLen(7, pbLen(1, []byte("call_1")))) }

type sink struct {
	mu   sync.Mutex
	sigs []string
	txt  []string
}

func (s *sink) add(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Signal != "" && (len(s.sigs) == 0 || s.sigs[len(s.sigs)-1] != r.Signal) {
		s.sigs = append(s.sigs, r.Signal)
	}
	for _, t := range r.Turns {
		s.txt = append(s.txt, t.Role+":"+t.Text)
	}
}

func (s *sink) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sigs) == 0 {
		return ""
	}
	return s.sigs[len(s.sigs)-1]
}

func (s *sink) turns() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.txt...)
}

func waitAgy(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func follow(t *testing.T, path string, skip bool) *sink {
	t.Helper()
	s := &sink{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go (&AgyDBTailer{Path: path, Fn: s.add, SkipExisting: skip, Every: 10 * time.Millisecond}).Run(ctx)
	return s
}

// The sequence below is what a real agy turn with one tool call writes
// (confirmed live): a 15 "done" row before every tool call is NOT the end of
// the turn - only the executor_metadata row is.
func TestAgyTailerFollowsARealTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	c := newConv(t, path, true)
	s := follow(t, path, false)
	c.step(0, 14, 3, userPayload("[relay | from lead | task | normal | msg 01K6AAAAAAAAAAAAAAAAAAAAAA]\nrun ls"))
	waitAgy(t, "busy", func() bool { return s.last() == SigAgyBusy })
	c.step(1, 15, 8, nil)
	c.status(1, 3)
	c.step(1, 15, 3, toolPlanPayload()) // the tool-call plan: done, turn still running
	c.step(2, 132, 9, nil)              // waiting for the user's permission
	waitAgy(t, "dialog", func() bool { return s.last() == SigAgyDialog })
	c.status(2, 3)
	waitAgy(t, "dialog cleared", func() bool { return s.last() == SigAgyDialogClear || s.last() == SigAgyBusy })
	c.step(3, 15, 3, answerPayload("done: 3 files"))
	time.Sleep(60 * time.Millisecond)
	if got := s.last(); got == SigAgyIdle {
		t.Fatal("reported idle before the turn ended")
	}
	c.endTurn(0, 4, 3)
	waitAgy(t, "idle", func() bool { return s.last() == SigAgyIdle })
	waitAgy(t, "turns", func() bool { return len(s.turns()) == 2 })
	got := s.turns()
	if !strings.HasPrefix(got[0], "user:[relay | from lead") || got[1] != "assistant:done: 3 files" {
		t.Fatalf("turns = %q", got)
	}
	if ids := MsgIDs(strings.TrimPrefix(got[0], "user:")); len(ids) != 1 {
		t.Fatalf("the relay header in the user turn must be recognised: %v", ids)
	}
}

func TestAgyTailerCancelledTurnEndsIdle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	c := newConv(t, path, true)
	s := follow(t, path, false)
	c.step(0, 14, 3, userPayload("go"))
	c.step(1, 15, 3, toolPlanPayload())
	c.step(2, 132, 9, nil)
	waitAgy(t, "dialog", func() bool { return s.last() == SigAgyDialog })
	c.status(2, 6) // Esc
	c.endTurn(0, 2, 2)
	waitAgy(t, "idle after cancel", func() bool { return s.last() == SigAgyIdle || s.last() == SigAgyDialogClear })
	waitAgy(t, "idle", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, sig := range s.sigs[len(s.sigs)-2:] {
			if sig == SigAgyIdle {
				return true
			}
		}
		return false
	})
}

// A background task finishing starts a turn with no user input (type 101).
func TestAgyTailerBackgroundTurnIsBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	c := newConv(t, path, true)
	c.step(0, 14, 3, userPayload("hi"))
	c.step(1, 15, 3, answerPayload("hello"))
	c.endTurn(0, 4, 1)
	s := follow(t, path, false)
	waitAgy(t, "idle", func() bool { return s.last() == SigAgyIdle })
	c.step(2, 101, 3, nil)
	waitAgy(t, "busy", func() bool { return s.last() == SigAgyBusy })
	c.step(3, 15, 3, answerPayload("bg done"))
	c.endTurn(1, 4, 3)
	waitAgy(t, "idle", func() bool { return s.last() == SigAgyIdle })
}

func TestAgyTailerSkipsHistoryOfAResumedConversation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	c := newConv(t, path, true)
	c.step(0, 14, 3, userPayload("old question"))
	c.step(1, 15, 3, answerPayload("old answer"))
	c.endTurn(0, 4, 1)
	s := follow(t, path, true)
	waitAgy(t, "idle", func() bool { return s.last() == SigAgyIdle })
	c.step(2, 14, 3, userPayload("new question"))
	waitAgy(t, "new turn", func() bool { return len(s.turns()) == 1 })
	if got := s.turns(); got[0] != "user:new question" {
		t.Fatalf("turns = %q, want only the new one", got)
	}
}

// Re-sent periodically, so a consumer's short TTL never lapses while agy
// sits idle, and a stopped tailer's state does lapse.
func TestAgyTailerReassertsState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	c := newConv(t, path, true)
	c.step(0, 14, 3, userPayload("hi"))
	c.endTurn(0, 4, 0)
	var mu sync.Mutex
	n := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&AgyDBTailer{Path: path, Every: 10 * time.Millisecond, Fn: func(r Record) {
		if r.Signal == SigAgyIdle {
			mu.Lock()
			n++
			mu.Unlock()
		}
	}}).Run(ctx)
	time.Sleep(agyReassert + 500*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n < 2 {
		t.Fatalf("idle sent %d times in %v, want a re-send", n, agyReassert+500*time.Millisecond)
	}
}

// agy checkpoints and drops -wal/-shm when it closes a conversation; a
// read-only open then fails (confirmed live) unless the file is opened as
// immutable. Paths with URI-special characters must work too.
func TestAgyTailerReadsAClosedDatabaseAtAnAwkwardPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b#c?d%e")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "conv.db")
	c := newConv(t, path, false)
	c.step(0, 14, 3, userPayload("q"))
	c.step(1, 15, 3, answerPayload("a"))
	c.endTurn(0, 4, 1)
	c.db.Close()
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Fatal("fixture should have no -wal")
	}
	if !strings.Contains(AgyDBURI(path), "immutable=1") {
		t.Fatalf("URI %q should open a closed database as immutable", AgyDBURI(path))
	}
	s := follow(t, path, false)
	waitAgy(t, "turns from a closed database", func() bool { return len(s.turns()) == 2 })
	waitAgy(t, "idle", func() bool { return s.last() == SigAgyIdle })
}

func TestAgyTailerMissingFileIsANoOp(t *testing.T) {
	s := follow(t, filepath.Join(t.TempDir(), "missing.db"), false)
	time.Sleep(100 * time.Millisecond)
	if s.last() != "" || len(s.turns()) != 0 {
		t.Fatalf("got %q %q from a missing database", s.sigs, s.txt)
	}
}

func TestPBReader(t *testing.T) {
	msg := append(pbVar(1, 4), pbLen(5, pbLen(1, pbVar(1, 300)), pbLen(9, []byte("x")))...)
	msg = append(msg, pbLen(19, pbLen(2, []byte("hello")))...)
	if v, ok := pbUint(msg, 1); !ok || v != 4 {
		t.Fatalf("field 1 = %d %v", v, ok)
	}
	if v, ok := pbUint(msg, 5, 1, 1); !ok || v != 300 {
		t.Fatalf("field 5.1.1 = %d %v", v, ok)
	}
	if s, ok := pbString(msg, 19, 2); !ok || s != "hello" {
		t.Fatalf("field 19.2 = %q %v", s, ok)
	}
	if _, ok := pbString(msg, 7); ok {
		t.Fatal("a missing field was found")
	}
	for _, junk := range [][]byte{nil, {0xff}, {0x0a, 0x7f}, {0x08}, {0x0a, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80}} {
		pbString(junk, 1) // must not panic
		pbUint(junk, 1, 2)
	}
}

func TestAgyUserTurnsReadsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	c := newConv(t, path, true)
	c.step(0, 14, 3, userPayload("the briefing"))
	c.step(1, 15, 3, answerPayload("ok"))
	c.step(2, 14, 8, userPayload("still being written"))
	got := AgyUserTurns(context.Background(), path)
	if len(got) != 1 || got[0] != "the briefing" {
		t.Fatalf("%q", got)
	}
	if got := AgyUserTurns(context.Background(), filepath.Join(t.TempDir(), "missing.db")); len(got) != 0 {
		t.Fatalf("%q from a missing database", got)
	}
}
