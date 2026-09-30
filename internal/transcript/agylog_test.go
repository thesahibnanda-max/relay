package transcript

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Lines captured live from agy 1.2.12/1.2.13's own log.
func TestParseAgyLogLine(t *testing.T) {
	cases := []struct {
		line string
		want AgyLogEvent
		ok   bool
	}{
		{`I0930 03:17:17.904660     387 server.go:1248] Created conversation 938faa13-bb0d-4fe0-9a4d-2218dab07166`,
			AgyLogEvent{Conversation: "938faa13-bb0d-4fe0-9a4d-2218dab07166", Created: true, Goroutine: "387"}, true},
		{`I0930 03:02:42.888371       1 common.go:401] Resuming conversation b70117b6-85fa-4e46-b572-452adb0b2af6` + "\n",
			AgyLogEvent{Conversation: "b70117b6-85fa-4e46-b572-452adb0b2af6", Goroutine: "1"}, true},
		{`I0930 03:17:26.232245     605 input_loop.go:107] HandleUserInput called with text: "second convo, reply \"OK2\"\n"`,
			AgyLogEvent{Input: "second convo, reply \"OK2\"\n", HasInput: true}, true},
		{`I0930 03:17:17.906672     387 conversation_manager.go:887] Streaming conversation 938faa13-bb0d-4fe0-9a4d-2218dab07166`, AgyLogEvent{}, false},
		{`I0930 03:06:22 ... GetConversationDetail: found conversation 3ffc6a0c-e597-4454-9c96-781af811b7a7 (active=true)`, AgyLogEvent{}, false},
		{``, AgyLogEvent{}, false},
	}
	for _, c := range cases {
		got, ok := ParseAgyLogLine(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("%q = %+v %v, want %+v %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestAgyLogFollowerTailsAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agy.log")
	var mu sync.Mutex
	var got []AgyLogEvent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&AgyLogFollower{Path: path, Every: 10 * time.Millisecond, Fn: func(e AgyLogEvent) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	}}).Run(ctx)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("I0930 x] Created conversation 938faa13-bb0d-4fe0-9a4d-2218dab07166\nnoise\n")
	f.WriteString("I0930 x] HandleUserInput called with text: \"par") // a line written in two parts
	time.Sleep(50 * time.Millisecond)
	f.WriteString("tial\"\n")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Conversation == "" || got[1].Input != "partial" {
		t.Fatalf("events = %+v", got)
	}
}

func TestAgyConversationDBUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	want := filepath.Join(home, ".gemini", "antigravity-cli", "conversations", "x.db")
	if got := AgyConversationDB("x"); got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}

func TestParseAgyLogLineSubagentStart(t *testing.T) {
	ev, ok := ParseAgyLogLine(`I0930 12:29:46.231549     302 conversation_manager.go:512] Starting new conversation (agent=true)`)
	if !ok || !ev.SubagentStart || ev.Goroutine != "302" {
		t.Fatalf("%+v %v", ev, ok)
	}
	if _, ok := ParseAgyLogLine(`I0930 12:29:46.231549     302 conversation_manager.go:512] Starting new conversation (agent=false)`); ok {
		t.Fatal("a main conversation start is not an event")
	}
	ev, _ = ParseAgyLogLine(`I0930 12:29:46.254904     302 server.go:1248] Created conversation 18543113-0cbd-49ba-8bfd-d1b56e58b8c3`)
	if ev.Goroutine != "302" || !ev.Created {
		t.Fatalf("%+v", ev)
	}
}

func TestParseAgyLogLineVersion(t *testing.T) {
	ev, ok := ParseAgyLogLine(`I0930 12:51:53.658619      53 server.go:1637] Language server version: 1.2.14`)
	if !ok || ev.Version != "1.2.14" {
		t.Fatalf("%+v %v", ev, ok)
	}
}

func TestAgyVersionTested(t *testing.T) {
	for v, want := range map[string]bool{"1.2.12": true, "1.2.14": true, "1.2.11": false, "1.2.15": false, "1.3.0": false, "2.0": false, "x": false} {
		if got := AgyVersionTested(v); got != want {
			t.Errorf("AgyVersionTested(%q) = %v", v, got)
		}
	}
}

// The follower counts what it reads, so a log whose lines relay no longer
// recognises is noticed rather than silently ignored.
func TestAgyLogFollowerCountsLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agy.log")
	os.WriteFile(path, []byte("I0930 12:51:53.657045      53 server.go:1586] Starting\nsomething else entirely\n"), 0o644)
	f := &AgyLogFollower{Path: path, Fn: func(AgyLogEvent) {}, Every: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lines, glog := f.Seen(); lines == 2 && glog == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines, glog := f.Seen()
	t.Fatalf("lines %d glog %d, want 2 1", lines, glog)
}

func TestAgyLogFollowerStartsAtFrom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agy.log")
	old := "I0930 03:17:17.904660     387 server.go:1248] Created conversation 938faa13-bb0d-4fe0-9a4d-2218dab07166\n"
	os.WriteFile(path, []byte(old+"I0930 03:17:18.000000     387 server.go:1248] Created conversation b70117b6-85fa-4e46-b572-452adb0b2af6\n"), 0o644)
	var mu sync.Mutex
	var got []string
	f := &AgyLogFollower{Path: path, From: int64(len(old)), Every: 10 * time.Millisecond, Fn: func(ev AgyLogEvent) {
		mu.Lock()
		got = append(got, ev.Conversation)
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "b70117b6-85fa-4e46-b572-452adb0b2af6" {
		t.Fatalf("got %q, want only the conversation after From", got)
	}
}
