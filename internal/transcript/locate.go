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

// CodexHome is where Codex keeps its data ($CODEX_HOME or ~/.codex).
func CodexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".codex")
	}
	return ""
}

// CodexQuery describes the Codex session to find.
type CodexQuery struct {
	Root    string    // sessions directory (default CodexHome()/sessions)
	Cwd     string    // the agent's working directory
	Since   time.Time // the agent started at this time
	Name    string    // agent name: the briefing mentions it (strong match)
	Session string    // relay session id: the briefing mentions it
}

// LocateCodex finds the rollout file belonging to this agent, or "". A rollout
// appears when the first message is sent, so callers poll. It matches on the
// working directory and start time, and, when several Codex agents share a
// directory, on the relay briefing (which names the agent and session) that
// Relay delivered as developer instructions.
func LocateCodex(q CodexQuery) string {
	root := q.Root
	if root == "" {
		root = filepath.Join(CodexHome(), "sessions")
	}
	var candidates []string
	days := map[string]bool{}
	for _, d := range []time.Time{q.Since, time.Now()} {
		days[filepath.Join(root, d.Format("2006"), d.Format("01"), d.Format("02"))] = true
	}
	for dir := range days {
		files, _ := filepath.Glob(filepath.Join(dir, "rollout-*.jsonl"))
		for _, f := range files {
			if st, err := os.Stat(f); err == nil && st.ModTime().After(q.Since.Add(-2*time.Second)) {
				candidates = append(candidates, f)
			}
		}
	}
	sort.Strings(candidates)
	wantCwd := realPath(q.Cwd)
	var byCwd []string
	for _, f := range candidates {
		meta, briefing := readRolloutHead(f)
		if meta == "" || realPath(meta) != wantCwd {
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
	if len(byCwd) == 1 && !hasRelayBriefing(byCwd[0]) {
		// One candidate and no relay briefing naming somebody else: it is ours.
		return byCwd[0]
	}
	return ""
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// readRolloutHead returns the session's cwd and the text of its developer message.
func readRolloutHead(path string) (cwd, developer string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 4<<20)
	for i := 0; i < 40 && sc.Scan(); i++ {
		var l struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case "session_meta":
			var m struct {
				Cwd string `json:"cwd"`
			}
			json.Unmarshal(l.Payload, &m)
			cwd = m.Cwd
		case "response_item":
			var m struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(l.Payload, &m) == nil && m.Type == "message" && m.Role == "developer" {
				for _, c := range m.Content {
					developer += c.Text
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		// an oversized or unreadable line: what was read so far is all we know
		return cwd, developer
	}
	return cwd, developer
}

// hasRelayBriefing reports whether a rollout carries a Relay briefing, i.e.
// belongs to some relay agent (which, if it were ours, matched by name above).
func hasRelayBriefing(path string) bool {
	_, dev := readRolloutHead(path)
	return strings.Contains(dev, "Relay session")
}
