package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

func writeSegments(t *testing.T, e *env, sid, aid string, n int) string {
	t.Helper()
	dir := filepath.Join(e.paths.RawDir(), sid, aid)
	os.MkdirAll(dir, 0o700)
	for i := 1; i <= n; i++ {
		body := strings.Repeat(`{"seq":1,"type":"out","b":"aGVsbG8gd29ybGQ="}`+"\n", 500)
		os.WriteFile(filepath.Join(dir, "raw-000"+string(rune('0'+i))+".jsonl"), []byte(body), 0o600)
	}
	return dir
}

func TestCompressRoundTripKeepsEveryByte(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raw-0001.jsonl")
	orig := bytes.Repeat([]byte(`{"seq":7,"type":"out","b":"AAAA"}`+"\n"), 1000)
	os.WriteFile(path, orig, 0o600)
	saved, err := compressFile(path)
	if err != nil || saved <= 0 {
		t.Fatalf("saved %d, %v", saved, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("original must be removed")
	}
	// os.Chmod cannot express owner-only on Windows (confirmed elsewhere: it
	// only toggles the read-only attribute, always reporting back 0666).
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path + ".zst"); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	f, _ := os.Open(path + ".zst")
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var got bytes.Buffer
	got.ReadFrom(dec)
	if !bytes.Equal(got.Bytes(), orig) {
		t.Fatal("decompressed data differs")
	}
}

func TestRotationCompressesClosedSegmentsAndResumeNumbersCorrectly(t *testing.T) {
	dir := t.TempDir()
	var bg bgCompress
	rw, err := openRaw(dir, "S", "A", 0)
	if err != nil {
		t.Fatal(err)
	}
	rw.bg = &bg
	if err := rw.rotate(); err != nil { // closes segment 1, opens 2
		t.Fatal(err)
	}
	rw.Write([]proto.Event{{Seq: 1, T: time.Now(), Type: "out", B: []byte("data")}})
	rw.Close()
	bg.wg.Wait()
	ad := filepath.Join(dir, "S", "A")
	if _, err := os.Stat(filepath.Join(ad, "raw-0001.jsonl.zst")); err != nil {
		t.Fatalf("closed segment should be compressed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ad, "raw-0002.jsonl")); err != nil {
		t.Fatalf("the open one stays plain: %v", err)
	}
	// a resumed agent continues after the highest segment, compressed or not
	rw2, _ := openRaw(dir, "S", "A", 0)
	if rw2.n != 2 {
		t.Fatalf("resumed at segment %d", rw2.n)
	}
	rw2.Close()
	os.Rename(filepath.Join(ad, "raw-0002.jsonl"), filepath.Join(ad, "raw-0002.jsonl.zst"))
	rw3, _ := openRaw(dir, "S", "A", 0)
	if rw3.n != 3 {
		t.Fatalf("with segment 2 compressed a new one (3) must start, got %d", rw3.n)
	}
	rw3.Close()
}

func TestGCPrunesIdleSessionsOnlyAndCompresses(t *testing.T) {
	e := startServer(t)
	idle := e.newSession()
	old := e.joinPeer(idle, proto.Hello{Name: "old", Role: "dev"})
	old.send(proto.TypeBye, proto.Bye{ExitCode: 0})
	e.waitAgent(idle, "old", func(a proto.AgentInfo) bool { return a.Status == "exited" }, "exit")
	oldDir := writeSegments(t, e, idle, old.w.Agent.ID, 3)

	live := e.newSession()
	l := e.joinPeer(live, proto.Hello{Name: "live", Role: "dev"})
	// Only one segment: a real, still-connected agent that hasn't rotated
	// only ever has its one open segment on disk. GC's keepOpen logic
	// protects exactly the newest (highest-numbered) segment for such an
	// agent, on the assumption - true in real use, since rotation only ever
	// increases segment numbers - that it's the one actually open; writing
	// extra, out-of-band segments here would violate that assumption and
	// have GC try to compress/remove this real, still-open file (confirmed
	// live: on Windows, unlike Unix, that always fails - the daemon's own
	// still-open handle blocks the delete for as long as the connection
	// stays up, i.e. for the rest of the test).
	liveDir := writeSegments(t, e, live, l.w.Agent.ID, 1)
	time.Sleep(1200 * time.Millisecond)

	// the session that still has a connected agent is never pruned, whatever its age
	rep := e.gc(proto.GCRequest{OlderThanS: 1, DryRun: true})
	if !rep.DryRun || rep.Sessions != 1 {
		t.Fatalf("dry run: %+v", rep)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Fatal("dry run must not touch anything")
	}

	// compress: the exited agent's segments all go; the connected agent's only (open) segment stays
	rep = e.gc(proto.GCRequest{Compress: true})
	if rep.SegmentsCompressed != 3 || rep.BytesSaved <= 0 {
		t.Fatalf("compress: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(liveDir, "raw-0001.jsonl")); err != nil {
		t.Fatal("the connected agent's open segment must stay writable")
	}
	if _, err := os.Stat(filepath.Join(oldDir, "raw-0003.jsonl.zst")); err != nil {
		t.Fatal("an exited agent's segments are all compressed")
	}
	if again := e.gc(proto.GCRequest{Compress: true}); again.SegmentsCompressed != 0 {
		t.Fatalf("idempotent: %+v", again)
	}
}

func TestGCDeletesAnIdleSessionCompletely(t *testing.T) {
	e := startServer(t)
	sid := e.newSession()
	a := e.joinPeer(sid, proto.Hello{Name: "alice", Role: "dev"})
	b := e.joinPeer(sid, proto.Hello{Name: "bob", Role: "dev"})
	a.mustSend(proto.SendArgs{To: "bob", Body: "hi"})
	b.mustDeliver()
	dir := writeSegments(t, e, sid, a.w.Agent.ID, 1)
	a.send(proto.TypeBye, proto.Bye{})
	b.send(proto.TypeBye, proto.Bye{})
	e.waitAgent(sid, "alice", func(x proto.AgentInfo) bool { return x.Status == "exited" }, "exit")
	e.waitAgent(sid, "bob", func(x proto.AgentInfo) bool { return x.Status == "exited" }, "exit")

	keep := e.newSession()
	k := e.joinPeer(keep, proto.Hello{Name: "keeper", Role: "dev"}) // connected: stays
	_ = k
	time.Sleep(1200 * time.Millisecond)

	rep := e.gc(proto.GCRequest{OlderThanS: 1})
	if rep.Sessions != 1 || rep.Agents != 2 || rep.Messages != 2 /* alice's message + the failure notice to her */ || rep.RawBytesFreed == 0 {
		t.Fatalf("report %+v", rep)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("raw logs must go with the session")
	}
	if code := e.getJSON("/v1/admin/sessions/"+sid, nil); code != 404 {
		t.Fatalf("session should be gone, got %d", code)
	}
	if code := e.getJSON("/v1/admin/sessions/"+keep, nil); code != 200 {
		t.Fatalf("a live session must survive: %d", code)
	}
	if rows, _ := e.srv.st.Turns(bg, a.w.Agent.ID, storeTurnQuery()); len(rows) != 0 {
		t.Fatal("events remain")
	}
}

func (e *env) gc(req proto.GCRequest) proto.GCReport {
	e.t.Helper()
	var rep proto.GCReport
	if code := e.postJSON("/v1/admin/gc", req, &rep); code != 200 {
		e.t.Fatalf("gc: %d", code)
	}
	return rep
}

func storeTurnQuery() store.TurnQuery { return store.TurnQuery{Limit: 10} }
