package transcript

import (
	"context"
	"database/sql"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, matching internal/store's own usage
)

// agy's `steps` table schema, confirmed live against real conversation
// databases (~/.gemini/antigravity-cli/conversations/<uuid>.db) on agy
// 1.2.11: idx (primary key, monotonically increasing), step_type, status,
// step_payload (BLOB), among other BLOB columns this package does not need.
//
// step_type/status enum values confirmed live:
//
//	stepTypeUserMessage      = 14  - starts a turn
//	stepTypeAssistantMessage = 15  - status=3 here (and only here) means the
//	                                 whole turn is done
//	stepTypeToolCall         = 132 - can also reach status=3 mid-turn without
//	                                 the turn being over: idle detection must
//	                                 key off step_type==15 specifically, never
//	                                 status alone.
const (
	agyStepUserMessage      = 14
	agyStepAssistantMessage = 15
	agyStatusComplete       = 3
)

// SQLiteTailer follows one of agy's per-conversation SQLite databases. Unlike
// every other adaptor's transcript (an append-only JSONL file, handled by the
// line-offset Tailer), agy's `steps` table is mutated in place, not appended
// to - so this polls full snapshots instead: each poll re-checks the
// last-seen row's own (idx, step_type, status), since that one row can still
// change after first being seen, and processes any row with a higher idx as
// new.
type SQLiteTailer struct {
	Path  string
	Fn    func(Record)
	Every time.Duration // poll interval (default 250ms)
}

func (t *SQLiteTailer) Run(ctx context.Context) {
	every := t.Every
	if every == 0 {
		every = 250 * time.Millisecond
	}
	var lastIdx int64 = -1
	var lastStepType, lastStatus int
	for {
		t.poll(ctx, &lastIdx, &lastStepType, &lastStatus)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (t *SQLiteTailer) poll(ctx context.Context, lastIdx *int64, lastStepType, lastStatus *int) {
	db, err := sql.Open("sqlite", "file:"+t.Path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT idx, step_type, status FROM steps WHERE idx >= ? ORDER BY idx`, *lastIdx)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var idx int64
		var stepType, status int
		if rows.Scan(&idx, &stepType, &status) != nil {
			continue
		}
		if idx == *lastIdx && stepType == *lastStepType && status == *lastStatus {
			continue // the one row we might re-see: unchanged since last poll
		}
		*lastIdx, *lastStepType, *lastStatus = idx, stepType, status
		if rec := agyRecord(stepType, status); rec.Signal != "" {
			t.Fn(rec)
		}
	}
}

func agyRecord(stepType, status int) Record {
	switch {
	case stepType == agyStepUserMessage:
		return Record{Signal: SigTaskStarted}
	case stepType == agyStepAssistantMessage && status == agyStatusComplete:
		return Record{Signal: SigTaskComplete}
	}
	return Record{}
}
