package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/thesahibnanda-max/relay/internal/proto"
)

// rotateSize is when a raw segment file is closed and a new one started.
const rotateSize = 16 << 20

// rawWriter appends an agent's raw terminal events to segment files
// (data/raw/<session>/<agent>/raw-0001.jsonl, ...), one JSON object per line.
// Raw bytes are bulky and only ever read sequentially, so they stay out of SQLite.
type rawWriter struct {
	dir  string
	n    int
	f    *os.File
	w    *bufio.Writer
	size int64
	enc  *json.Encoder

	bg      *bgCompress
	limit   int64 // total raw bytes kept for this agent (0 = unlimited)
	total   int64 // bytes written across all segments, including earlier runs
	dropped uint64
}

// Dropped is how many raw events were not stored because the quota was reached.
func (r *rawWriter) Dropped() uint64 { return r.dropped }

type rawLine struct {
	Seq  uint64    `json:"seq"`
	T    time.Time `json:"t"`
	Type string    `json:"type"`
	B    []byte    `json:"b"`
}

func openRaw(root, session, agent string, limit int64) (*rawWriter, error) {
	dir := filepath.Join(root, session, agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Continue after the highest existing segment so a resumed agent appends.
	n := 0
	matches, _ := filepath.Glob(filepath.Join(dir, "raw-*.jsonl*")) // includes compressed .zst segments
	for _, m := range matches {
		var i int
		if _, err := fmt.Sscanf(filepath.Base(m), "raw-%04d.jsonl", &i); err == nil && i > n {
			n = i
		}
	}
	rw := &rawWriter{dir: dir, n: n, limit: limit}
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil {
			rw.total += st.Size()
		}
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("raw-%04d.jsonl", n))); n > 0 && err == nil {
		if err := rw.openSegment(n); err != nil { // reuse the last one
			return nil, err
		}
	} else if err := rw.rotate(); err != nil { // none yet, or the last was already compressed
		return nil, err
	}
	return rw, nil
}

func (r *rawWriter) openSegment(i int) error {
	f, err := os.OpenFile(filepath.Join(r.dir, fmt.Sprintf("raw-%04d.jsonl", i)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	r.f, r.n, r.size = f, i, st.Size()
	r.w = bufio.NewWriterSize(f, 64<<10)
	r.enc = json.NewEncoder(r.w)
	return nil
}

func (r *rawWriter) rotate() error {
	if r.f != nil {
		r.w.Flush()
		r.f.Close()
		if r.bg != nil {
			r.bg.run(r.f.Name()) // closed segments are compressed in the background
		}
	}
	return r.openSegment(r.n + 1)
}

// Write appends the raw events in evs (others are ignored); call Flush after a batch.
func (r *rawWriter) Write(evs []proto.Event) error {
	for _, e := range evs {
		if !e.IsRaw() {
			continue
		}
		if r.limit > 0 && r.total >= r.limit {
			r.dropped++ // quota reached: keep the tool running, stop recording its output
			continue
		}
		if r.size >= rotateSize {
			if err := r.rotate(); err != nil {
				return err
			}
		}
		if err := r.enc.Encode(rawLine{Seq: e.Seq, T: e.T, Type: e.Type, B: e.B}); err != nil {
			return err
		}
		r.size += int64(len(e.B)) + 64
		r.total += int64(len(e.B)) + 64
	}
	return nil
}

func (r *rawWriter) Flush() error { return r.w.Flush() }

func (r *rawWriter) Close() error {
	if r.f == nil {
		return nil
	}
	err := r.w.Flush()
	if cerr := r.f.Close(); err == nil {
		err = cerr
	}
	r.f = nil
	return err
}
