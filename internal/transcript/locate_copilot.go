package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CopilotHome is where Copilot CLI keeps its session state ($COPILOT_HOME or ~/.copilot).
func CopilotHome() string {
	if h := os.Getenv("COPILOT_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".copilot")
	}
	return ""
}

// CopilotQuery describes the Copilot session to find.
type CopilotQuery struct {
	Root    string // session-state directory (default CopilotHome()/session-state)
	Cwd     string // the agent's working directory
	Since   time.Time
	Name    string // agent name: the typed briefing mentions it (strong match)
	Session string // relay session id: the typed briefing mentions it
}

// LocateCopilot finds the events.jsonl belonging to this agent, or "". A
// session directory appears when the first message is sent, so callers
// poll. Mirrors LocateCodex's cwd/mtime/briefing-text disambiguation,
// adapted to Copilot's flat session-state/{session-id}/events.jsonl layout
// (no date-bucketed subdirectories).
//
// Unlike Codex, Copilot's Prepare never delivers the briefing via a launch
// flag (see impl.go) - it always arrives as agentcmd.go's typed bootstrap
// message, i.e. as the session's first ordinary user message. So where
// LocateCodex disambiguates on a developer-role transcript message,
// LocateCopilot disambiguates on the first user.message event instead.
func LocateCopilot(q CopilotQuery) string {
	root := q.Root
	if root == "" {
		root = filepath.Join(CopilotHome(), "session-state")
	}
	dirs, _ := filepath.Glob(filepath.Join(root, "*"))
	var candidates []string
	for _, d := range dirs {
		f := filepath.Join(d, "events.jsonl")
		if st, err := os.Stat(f); err == nil && st.ModTime().After(q.Since.Add(-2*time.Second)) {
			candidates = append(candidates, f)
		}
	}
	sort.Strings(candidates)
	wantCwd := realPath(q.Cwd)
	var byCwd []string
	for _, f := range candidates {
		cwd, briefing := readCopilotHead(f)
		if cwd == "" || realPath(cwd) != wantCwd {
			continue
		}
		if q.Name != "" && q.Session != "" && strings.Contains(briefing, `"`+q.Name+`"`) && strings.Contains(briefing, q.Session) {
			return f
		}
		byCwd = append(byCwd, f)
	}
	if q.Name == "" && len(byCwd) == 1 {
		return byCwd[0]
	}
	if len(byCwd) == 1 && !hasCopilotRelayBriefing(byCwd[0]) {
		// One candidate and no relay briefing naming somebody else: it is ours.
		return byCwd[0]
	}
	return ""
}

// readCopilotHead returns the session's cwd (from session.start) and the
// concatenated text of its early user.message events. Field names verified
// live against copilot 1.0.88: every event nests its payload under "data".
func readCopilotHead(path string) (cwd, userText string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 4<<20)
	for i := 0; i < 40 && sc.Scan(); i++ {
		var l struct {
			Type string `json:"type"`
			Data struct {
				Context struct {
					Cwd string `json:"cwd"`
				} `json:"context"`
				Content string `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue // defensive: skip a corrupted line, never fail the scan
		}
		switch l.Type {
		case "session.start":
			cwd = l.Data.Context.Cwd
		case "user.message":
			userText += l.Data.Content
		}
	}
	if err := sc.Err(); err != nil {
		// an oversized or unreadable line: what was read so far is all we know
		return cwd, userText
	}
	return cwd, userText
}

// hasCopilotRelayBriefing reports whether a session carries a Relay
// briefing, i.e. belongs to some relay agent (which, if it were ours,
// matched by name above).
func hasCopilotRelayBriefing(path string) bool {
	_, text := readCopilotHead(path)
	return strings.Contains(text, "Relay session")
}
