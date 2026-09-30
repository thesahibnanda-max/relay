package main

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// agy's real conversation schema (1.2.12/1.2.13), trimmed to what matters.
const schema = "CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0,`status` integer NOT NULL DEFAULT 0,`has_subtrajectory` numeric NOT NULL DEFAULT false,`metadata` blob,`error_details` blob,`permissions` blob,`task_details` blob,`render_info` blob,`step_payload` blob,`step_format` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`));" +
	"CREATE TABLE `executor_metadata` (`idx` integer,`data` blob,PRIMARY KEY (`idx`));" +
	"CREATE TABLE `trajectory_metadata_blob` (`id` text DEFAULT \"main\",`data` blob,PRIMARY KEY (`id`));"

const (
	stepUser  = 14
	stepModel = 15
	stepTool  = 132
	stepEvent = 101

	statusDone      = 3
	statusCancelled = 6
	statusGenerate  = 8
	statusAsk       = 9
)

type conversation struct {
	id      string
	db      *sql.DB
	next    int // next step idx
	turns   int // executor_metadata rows
	turnEnd int // idx of the last finished turn's last step (-1: none)
}

func convDir() string { return filepath.Join(geminiDir(), "antigravity-cli", "conversations") }

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func openDB(path string) (*sql.DB, error) {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// createConversation makes a new conversation database for workspace cwd.
func createConversation(cwd string) (*conversation, error) {
	if err := os.MkdirAll(convDir(), 0o755); err != nil {
		return nil, err
	}
	c := &conversation{id: newUUID(), turnEnd: -1}
	db, err := openDB(filepath.Join(convDir(), c.id+".db"))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(cwd)}).String()
	meta := cat(pbLen(1, pbLen(1, []byte(uri))), pbLen(3, []byte(newUUID())), pbLen(6, []byte(c.id)), pbLen(7, []byte(uri)))
	if _, err := db.Exec("INSERT INTO trajectory_metadata_blob(id, data) VALUES('main', ?)", meta); err != nil {
		db.Close()
		return nil, err
	}
	c.db = db
	return c, nil
}

// resumeConversation reopens conversation id, or the most recent one if id
// is empty (-c).
func resumeConversation(id string) (*conversation, error) {
	if id == "" {
		files, _ := filepath.Glob(filepath.Join(convDir(), "*.db"))
		sort.Slice(files, func(i, j int) bool {
			a, _ := os.Stat(files[i])
			b, _ := os.Stat(files[j])
			return a.ModTime().After(b.ModTime())
		})
		if len(files) == 0 {
			return nil, fmt.Errorf("no conversation to continue")
		}
		id = strings.TrimSuffix(filepath.Base(files[0]), ".db")
	}
	db, err := openDB(filepath.Join(convDir(), id+".db"))
	if err != nil {
		return nil, err
	}
	c := &conversation{id: id, db: db, turnEnd: -1}
	db.QueryRow("SELECT COALESCE(MAX(idx)+1, 0) FROM steps").Scan(&c.next)
	db.QueryRow("SELECT COUNT(*) FROM executor_metadata").Scan(&c.turns)
	c.turnEnd = c.next - 1
	return c, nil
}

func (c *conversation) close() {
	if c != nil && c.db != nil {
		c.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		c.db.Close()
	}
}

func now() []byte {
	t := time.Now()
	return pbLen(5, pbLen(1, pbVar(1, uint64(t.Unix())), pbVar(2, uint64(t.Nanosecond()))))
}

// step appends a step and returns its idx.
func (c *conversation) step(typ, status int, payload []byte) int {
	idx := c.next
	c.next++
	c.db.Exec("INSERT INTO steps(idx, step_type, status, step_payload) VALUES(?,?,?,?)", idx, typ, status, cat(now(), payload))
	return idx
}

func (c *conversation) setStatus(idx, status int) {
	c.db.Exec("UPDATE steps SET status=? WHERE idx=?", status, idx)
}

func (c *conversation) setPayload(idx int, payload []byte) {
	c.db.Exec("UPDATE steps SET step_payload=? WHERE idx=?", cat(now(), payload), idx)
}

// endTurn writes agy's end-of-turn row: outcome 4 completed, 2 cancelled.
func (c *conversation) endTurn(outcome int) {
	last := c.next - 1
	data := cat(pbVar(1, uint64(outcome)), pbVar(2, 1), pbVar(3, uint64(last)), pbLen(9, []byte(newUUID())))
	c.db.Exec("INSERT INTO executor_metadata(idx, data) VALUES(?,?)", c.turns, data)
	c.turns++
	c.turnEnd = last
}

func userPayload(text string) []byte {
	return pbLen(19, pbLen(2, []byte(text)), pbLen(3, pbLen(1, []byte(text))))
}
func answerPayload(text string) []byte {
	return pbLen(20, pbLen(1, []byte(text)), pbLen(8, []byte(text)))
}
func toolPlanPayload(name string) []byte {
	return pbLen(20, pbLen(7, pbLen(1, []byte("call_"+newUUID()[:8])), pbLen(2, []byte(name))))
}

// ---- protobuf wire encoding ------------------------------------------------------

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func pbUvarint(n uint64) []byte {
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	return append(out, byte(n))
}

func pbVar(field int, n uint64) []byte { return cat(pbUvarint(uint64(field<<3)), pbUvarint(n)) }

func pbLen(field int, parts ...[]byte) []byte {
	body := cat(parts...)
	return cat(pbUvarint(uint64(field<<3|2)), pbUvarint(uint64(len(body))), body)
}
