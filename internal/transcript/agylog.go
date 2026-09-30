package transcript

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// agy writes a diagnostic log (relay gives each launch its own with
// --log-file). It names, authoritatively, the conversation this very process
// is on - at start, after /new (which creates a new conversation) and on
// resume - and every prompt it accepts. Confirmed live, agy 1.2.12/1.2.13:
//
//	... server.go:1248] Created conversation 938faa13-bb0d-4fe0-9a4d-2218dab07166
//	... common.go:401] Resuming conversation b70117b6-85fa-4e46-b572-452adb0b2af6
//	... input_loop.go:107] HandleUserInput called with text: "second convo, reply OK2"
var (
	agyLogConversation = regexp.MustCompile(`\] (Created|Resuming) conversation ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\s*$`)
	agyLogInput        = regexp.MustCompile(`\] HandleUserInput called with text: (".*")\s*$`)
	agyLogStart        = regexp.MustCompile(`\] Starting new conversation \(agent=(true|false)\)`)
	agyLogVersion      = regexp.MustCompile(`\] Language server version: (\S+)\s*$`)
	// agyLogGoroutine is the glog header's goroutine column:
	// "I0930 12:29:46.254904     302 server.go:1248] ...".
	agyLogGoroutine = regexp.MustCompile(`^[IWEF]\d{4} [\d:.]+\s+(\d+) `)
)

// AgyLogEvent is one thing agy's log says.
type AgyLogEvent struct {
	Conversation string // a conversation this process is now on
	Created      bool   // ...that it just created (vs resumed)
	Input        string // a prompt agy accepted
	HasInput     bool
	// SubagentStart: an agent (subagent) conversation is being started on
	// Goroutine; the conversation that goroutine creates next is not the
	// one on screen.
	SubagentStart bool
	Goroutine     string
	Version       string // agy's version, logged once at startup
}

// The agy versions relay was verified against, live.
const (
	AgyTestedMin = "1.2.12"
	AgyTestedMax = "1.2.14"
)

// AgyVersionTested reports whether agy version v is in the tested range.
func AgyVersionTested(v string) bool {
	n, ok := versionNums(v)
	lo, _ := versionNums(AgyTestedMin)
	hi, _ := versionNums(AgyTestedMax)
	return ok && cmpVersion(n, lo) >= 0 && cmpVersion(n, hi) <= 0
}

func versionNums(v string) ([3]int, bool) {
	var n [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

func cmpVersion(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ParseAgyLogLine reads one log line.
func ParseAgyLogLine(line string) (AgyLogEvent, bool) {
	gid := ""
	if m := agyLogGoroutine.FindStringSubmatch(line); m != nil {
		gid = m[1]
	}
	if m := agyLogVersion.FindStringSubmatch(line); m != nil {
		return AgyLogEvent{Version: m[1]}, true
	}
	if m := agyLogStart.FindStringSubmatch(line); m != nil {
		return AgyLogEvent{SubagentStart: m[1] == "true", Goroutine: gid}, m[1] == "true"
	}
	if m := agyLogConversation.FindStringSubmatch(line); m != nil {
		return AgyLogEvent{Conversation: m[2], Created: m[1] == "Created", Goroutine: gid}, true
	}
	if m := agyLogInput.FindStringSubmatch(line); m != nil {
		text, err := strconv.Unquote(m[1])
		if err != nil {
			text = m[1]
		}
		return AgyLogEvent{Input: text, HasInput: true}, true
	}
	return AgyLogEvent{}, false
}

// GeminiHome is agy's data directory. agy derives it from the user's home
// directory and honours no override (confirmed: no GEMINI_HOME in the
// binary), so neither does relay.
func GeminiHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".gemini")
	}
	return ""
}

// AgyConversationDB is where agy keeps conversation id's database.
func AgyConversationDB(id string) string {
	return filepath.Join(GeminiHome(), "antigravity-cli", "conversations", id+".db")
}

// AgyLogFollower tails agy's log from the beginning, calling Fn per event.
type AgyLogFollower struct {
	Path  string
	Fn    func(AgyLogEvent)
	Every time.Duration // poll interval (default 200ms)
	From  int64         // offset to start reading at

	lines, glog atomic.Int64
}

// Seen reports how many complete lines were read, and how many of them had
// the glog header every agy log line has.
func (f *AgyLogFollower) Seen() (lines, glog int64) { return f.lines.Load(), f.glog.Load() }

func (f *AgyLogFollower) Run(ctx context.Context) {
	every := f.Every
	if every == 0 {
		every = 200 * time.Millisecond
	}
	off := f.From
	var partial []byte
	for {
		if file, err := os.Open(f.Path); err == nil {
			if st, err := file.Stat(); err == nil && st.Size() < off {
				off, partial = 0, nil // truncated or replaced: start over
			}
			if _, err := file.Seek(off, io.SeekStart); err == nil {
				r := bufio.NewReaderSize(file, 64<<10)
				for {
					chunk, err := r.ReadBytes('\n')
					off += int64(len(chunk))
					if len(partial)+len(chunk) > 1<<20 {
						partial = nil // a runaway line: drop it rather than grow forever
						continue
					}
					partial = append(partial, chunk...)
					if err != nil {
						break // incomplete last line: keep it for the next poll
					}
					f.lines.Add(1)
					if agyLogGoroutine.Match(partial) {
						f.glog.Add(1)
					}
					if ev, ok := ParseAgyLogLine(string(partial)); ok {
						f.Fn(ev)
					}
					partial = partial[:0]
				}
			}
			file.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
