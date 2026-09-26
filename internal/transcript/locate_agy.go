package transcript

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// GeminiHome is where agy keeps its data ($GEMINI_HOME or ~/.gemini).
// GEMINI_HOME is not confirmed to be honored by the real agy binary itself
// (unlike CODEX_HOME/COPILOT_HOME, which the real Codex/Copilot binaries do
// honor) - this only steers Relay's own bookkeeping and tests.
func GeminiHome() string {
	if h := os.Getenv("GEMINI_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".gemini")
	}
	return ""
}

// AgyQuery describes the agy conversation database to find.
type AgyQuery struct {
	Root    string // conversations directory (default GeminiHome()/antigravity-cli/conversations)
	Cwd     string // the agent's working directory
	Since   time.Time
	Name    string // agent name: the typed briefing mentions it (strong match)
	Session string // relay session id: the typed briefing mentions it
}

// LocateAgy finds the conversation database belonging to this agent, or "".
// A database appears when the first message is sent, so callers poll.
// Mirrors LocateCopilot's cwd/mtime/briefing-text disambiguation: agy, like
// Copilot, never delivers the relay briefing via a launch flag (see the agy
// adaptor's Prepare), so it always arrives as agentcmd.go's typed bootstrap
// message - the session's first ordinary user message - and that is what
// disambiguates several candidates sharing one cwd.
func LocateAgy(q AgyQuery) string {
	root := q.Root
	if root == "" {
		root = filepath.Join(GeminiHome(), "antigravity-cli", "conversations")
	}
	files, _ := filepath.Glob(filepath.Join(root, "*.db"))
	var candidates []string
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && st.ModTime().After(q.Since.Add(-2*time.Second)) {
			candidates = append(candidates, f)
		}
	}
	sort.Strings(candidates)
	wantCwd := realPath(q.Cwd)
	var byCwd []string
	for _, f := range candidates {
		if !agyDBMatchesCwd(f, wantCwd) {
			continue
		}
		if q.Name != "" && q.Session != "" {
			userText := agyUserText(f)
			if strings.Contains(userText, `"`+q.Name+`"`) && strings.Contains(userText, q.Session) {
				return f
			}
		}
		byCwd = append(byCwd, f)
	}
	if q.Name == "" && len(byCwd) == 1 {
		return byCwd[0]
	}
	if len(byCwd) == 1 && !hasAgyRelayBriefing(byCwd[0]) {
		// One candidate and no relay briefing naming somebody else: it is ours.
		return byCwd[0]
	}
	return ""
}

// printableRun finds runs of plain, printable ASCII text embedded in agy's
// undocumented, protobuf-shaped BLOB columns: cwd and message text are both
// recoverable this way without parsing that encoding - confirmed live by
// direct byte inspection of real conversation databases on this machine.
var printableRun = regexp.MustCompile(`[\x20-\x7e]{5,}`)

// agyDBMatchesCwd reports whether path's trajectory metadata (table
// trajectory_metadata_blob, the single row id="main") names wantCwd as its
// workspace. Confirmed live: that row's data blob contains a plain
// "file://<absolute path>" URI - the most reliable source of cwd this schema
// offers (far more reliable than scanning ordinary message payloads, which
// also contain unrelated paths from tool calls). Matched with HasPrefix, not
// equality: the printable-run scan cannot always tell exactly where the path
// ends before an adjacent protobuf field's own bytes begin (confirmed live:
// occasionally one trailing byte survives), so a path-separator-bounded
// prefix match is the safe comparison, not a guess at the exact cutoff.
func agyDBMatchesCwd(path, wantCwd string) bool {
	if wantCwd == "" {
		return false
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return false
	}
	defer db.Close()
	var blob []byte
	if db.QueryRow(`SELECT data FROM trajectory_metadata_blob WHERE id='main'`).Scan(&blob) != nil {
		return false
	}
	var candidates []string
	for _, m := range printableRun.FindAll(blob, -1) {
		i := strings.Index(string(m), "file://")
		if i < 0 {
			continue
		}
		candidates = append(candidates, fileURIToPath(string(m)[i+len("file://"):]))
	}
	// Prefer an exact match: confirmed live, the real blob usually contains
	// the URI twice, and at least one occurrence is typically un-contaminated
	// by an adjacent protobuf field's own bytes. Only fall back to a looser
	// prefix match (hasPathPrefix) when every occurrence found has something
	// appended to it.
	for _, c := range candidates {
		if c == wantCwd {
			return true
		}
	}
	for _, c := range candidates {
		if hasPathPrefix(c, wantCwd) {
			return true
		}
	}
	return false
}

// fileURIToPath converts the part of a file:// URI after the scheme to a
// filesystem path: URIs always use forward slashes and, for a Windows drive
// path, an extra leading slash before the drive letter (file:///C:/Users/...)
// - this is the standard file-URI convention, not agy-specific, but not yet
// live-verified against a real Windows agy install (see the plan's Windows/
// Unix parity notes).
func fileURIToPath(rest string) string {
	if len(rest) >= 3 && rest[0] == '/' && rest[2] == ':' {
		rest = rest[1:]
	}
	return filepath.FromSlash(rest)
}

// hasPathPrefix is the fallback used only when agyDBMatchesCwd found no exact
// match among its candidates: it reports whether candidate starts with want.
// Deliberately a bare string prefix, not separator-bounded: the noise
// confirmed live glued onto a real path (another protobuf field's own bytes,
// caught by the same printable-ASCII scan) is not reliably a path separator
// - a single trailing letter is just as likely - so requiring one would
// reject genuine matches. This trades a theoretical false positive between
// two real, differently-named directories where one's name is a strict
// prefix of the other's (e.g. "relay" vs "relay2") for not missing real
// matches; LocateAgy's caller-side recency window and, when more than one
// candidate remains, its briefing-text disambiguation both have to also
// agree before a match is ever used, which makes that combination of
// coincidences exceedingly unlikely in practice.
func hasPathPrefix(candidate, want string) bool {
	return strings.HasPrefix(candidate, want)
}

// agyUserText returns the concatenated printable text of a session's early
// user-message steps (step_type 14), used only for the relay-briefing
// substring checks above - approximate is fine, since those are Contains
// checks, not exact comparisons.
func agyUserText(path string) string {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return ""
	}
	defer db.Close()
	rows, err := db.Query(`SELECT step_payload FROM steps WHERE step_type=? ORDER BY idx LIMIT 20`, agyStepUserMessage)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var text strings.Builder
	for rows.Next() {
		var payload []byte
		if rows.Scan(&payload) != nil {
			continue
		}
		for _, m := range printableRun.FindAll(payload, -1) {
			text.Write(m)
			text.WriteByte('\n')
		}
	}
	return text.String()
}

// hasAgyRelayBriefing reports whether a session carries a Relay briefing,
// i.e. belongs to some relay agent (which, if it were ours, matched by name
// above).
func hasAgyRelayBriefing(path string) bool {
	return strings.Contains(agyUserText(path), "Relay session")
}
