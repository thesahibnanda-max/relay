package transcript

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, matching internal/store's own usage
)

// agy keeps each conversation in its own SQLite database
// (~/.gemini/antigravity-cli/conversations/<id>.db, WAL mode). Confirmed live
// against agy 1.2.12/1.2.13:
//
//   - steps(idx, step_type, status, step_payload, ...) grows by one row per
//     step; a row's status changes in place (8 generating -> 3 done, or 9
//     waiting for the user's permission -> 3 done / 6 cancelled). Types seen:
//     14 user input, 15 model output (answers and tool-call plans alike -
//     one is written, done, before every tool call), 132 tool execution, 101
//     a system/background event (which can start a turn on its own).
//   - executor_metadata gets exactly one row when a turn ends, completed or
//     cancelled; its field 3 is the turn's last step idx.
//
// So the conversation is busy exactly while a step exists past the last
// finished turn, and a step in status 9 is a permission prompt on screen.
const (
	agyStepUser   = 14
	agyStepModel  = 15
	agyStatusDone = 3
	agyStatusAsk  = 9 // waiting for the user to approve
)

// Signals only the agy tailer emits: its state is authoritative and re-sent
// periodically, so consumers give it a short TTL (see collab).
const (
	SigAgyBusy        = "agy_busy"
	SigAgyIdle        = "agy_idle"
	SigAgyDialog      = "agy_dialog"       // a step is waiting for the user's permission
	SigAgyDialogClear = "agy_dialog_clear" // ...and no longer is
)

// agyReassert is how often the tailer repeats an unchanged state, so a
// stopped tailer's last word cannot outlive it.
const agyReassert = 3 * time.Second

// AgyDBURI is the read-only SQLite URI for one of agy's databases, escaped
// so any path works. A live database has a -wal file and is opened
// read-only through it; once agy closes a conversation it checkpoints and
// removes -wal/-shm, and a read-only open of a WAL database without -shm
// fails ("unable to open database file", confirmed live) - that one is
// immutable, so it is opened as such.
func AgyDBURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive path: file:///C:/...
	}
	q := "mode=ro&_pragma=busy_timeout(3000)"
	if _, err := os.Stat(path + "-wal"); err != nil {
		q = "immutable=1"
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: q}).String()
}

// AgyDBTailer follows one agy conversation database.
type AgyDBTailer struct {
	Path string
	Fn   func(Record)
	// SkipExisting treats steps already in the database when following
	// starts as history: their turns are not reported (a resumed
	// conversation), only the state is.
	SkipExisting bool
	Every        time.Duration // poll interval (default 250ms)
}

type agyStep struct {
	idx            int64
	typ, status    int
	payloadPending bool
}

type agyTail struct {
	t        *AgyDBTailer
	db       *sql.DB
	dbURI    string
	seeded   bool
	reported map[int64]bool // steps whose turn was reported (or skipped as history)
	state    string
	dialog   bool
	lastSent time.Time
}

func (t *AgyDBTailer) Run(ctx context.Context) {
	every := t.Every
	if every == 0 {
		every = 250 * time.Millisecond
	}
	a := &agyTail{t: t, reported: map[int64]bool{}}
	defer a.close()
	for {
		a.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// AgyUserTurns reads, once, the text of every finished user turn in an agy
// conversation database (none if it cannot be read).
func AgyUserTurns(ctx context.Context, path string) []string {
	var out []string
	a := &agyTail{t: &AgyDBTailer{Path: path, Fn: func(r Record) {
		for _, t := range r.Turns {
			if t.Role == "user" {
				out = append(out, t.Text)
			}
		}
	}}, reported: map[int64]bool{}}
	defer a.close()
	a.poll(ctx)
	return out
}

func (a *agyTail) close() {
	if a.db != nil {
		a.db.Close()
		a.db = nil
	}
}

func (a *agyTail) open() bool {
	uri := AgyDBURI(a.t.Path)
	if a.db != nil && uri == a.dbURI {
		return true
	}
	a.close()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return false
	}
	db.SetMaxOpenConns(1)
	a.db, a.dbURI = db, uri
	return true
}

func (a *agyTail) poll(ctx context.Context) {
	if !a.open() {
		return
	}
	lastDone := int64(-1)
	var meta []byte
	switch err := a.db.QueryRowContext(ctx, `SELECT data FROM executor_metadata ORDER BY idx DESC LIMIT 1`).Scan(&meta); err {
	case nil:
		if v, ok := pbUint(meta, 3); ok {
			lastDone = int64(v)
		}
	case sql.ErrNoRows:
	default:
		a.close() // reopen next time (the database may have been checkpointed/closed)
		return
	}
	rows, err := a.db.QueryContext(ctx, `SELECT idx, step_type, status FROM steps ORDER BY idx`)
	if err != nil {
		a.close()
		return
	}
	var steps []agyStep
	for rows.Next() {
		var s agyStep
		if rows.Scan(&s.idx, &s.typ, &s.status) == nil {
			steps = append(steps, s)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		a.close()
		return
	}

	if !a.seeded {
		a.seeded = true
		if a.t.SkipExisting {
			for _, s := range steps {
				a.reported[s.idx] = true
			}
		}
	}

	var turns []Turn
	for _, s := range steps {
		if a.reported[s.idx] || s.status != agyStatusDone || (s.typ != agyStepUser && s.typ != agyStepModel) {
			continue
		}
		a.reported[s.idx] = true
		if t, ok := a.turn(ctx, s); ok {
			turns = append(turns, t)
		}
	}

	busy, dialog := false, false
	for _, s := range steps {
		if s.idx > lastDone {
			busy = true
			if s.status == agyStatusAsk {
				dialog = true
			}
		}
	}
	state := SigAgyIdle
	if busy {
		state = SigAgyBusy
	}
	rec := Record{Turns: turns, Fresh: len(steps) == 0}
	if state != a.state || time.Since(a.lastSent) >= agyReassert {
		rec.Signal, a.state, a.lastSent = state, state, time.Now()
	}
	if len(rec.Turns) > 0 || rec.Signal != "" {
		a.t.Fn(rec)
	}
	if dialog != a.dialog || (dialog && rec.Signal != "") {
		a.dialog = dialog
		sig := SigAgyDialogClear
		if dialog {
			sig = SigAgyDialog
		}
		a.t.Fn(Record{Signal: sig})
	}
}

// turn extracts the text of a finished user input (field 19.2) or model
// answer (field 20.1) step.
func (a *agyTail) turn(ctx context.Context, s agyStep) (Turn, bool) {
	var payload []byte
	if a.db.QueryRowContext(ctx, `SELECT step_payload FROM steps WHERE idx=?`, s.idx).Scan(&payload) != nil {
		return Turn{}, false
	}
	var text, role string
	switch s.typ {
	case agyStepUser:
		text, _ = pbString(payload, 19, 2)
		role = "user"
	case agyStepModel:
		text, _ = pbString(payload, 20, 1)
		role = "assistant"
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Turn{}, false
	}
	t := Turn{Role: role, Text: clip(text)}
	if sec, ok := pbUint(payload, 5, 1, 1); ok {
		nsec, _ := pbUint(payload, 5, 1, 2)
		t.TS = time.Unix(int64(sec), int64(nsec))
	}
	return t, true
}
