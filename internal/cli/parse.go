// Package cli parses the command line and runs the relay subcommands.
package cli

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thesahibnanda-max/relay/internal/adaptor"
	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/naming"
	"github.com/thesahibnanda-max/relay/internal/proto"
)

type Kind int

const (
	KindAgent Kind = iota // relay <tool> ...
	KindLs
	KindSessionNew
	KindSessionEnd
	KindSessionInvite // relay session invite <id>: mint a --join blob for an existing session
	KindSessionPeers  // relay session peers <id>: list what this daemon knows about a mesh session's peers
	KindDaemon
	KindDaemonStop
	KindDaemonStatus
	KindVersion
	KindHelp
	KindMCP      // relay mcp --dir <run dir>: the stdio MCP server a tool spawns
	KindApprove  // relay approve [ls|accept|reject] [id|all]
	KindSend     // relay send <agent> <text>: a message from the human
	KindMessages // relay messages: inspect what agents said to each other
	KindGC       // relay gc: remove stale per-launch files
	KindHook     // relay hook <event> --dir <run dir>: a Claude hook, run by Claude
	KindDoctor   // relay doctor: diagnose the installation
)

// Record says how much of the terminal stream is kept.
type Record string

const (
	RecordRaw    Record = "raw"    // everything (default)
	RecordEvents Record = "events" // structured events only: no terminal bytes
	RecordOff    Record = "off"    // nothing recorded; the daemon only sees presence
)

type Parsed struct {
	Kind Kind

	// KindAgent
	Tool           string
	Shim           bool            // invoked as a symlink named after the tool: no relay flags
	Role           string          // spec: builtin name or file path ("" = none)
	Session        string          // "" (solo), proto.SessionNew, or a normalised ULID
	Join           *proto.JoinBlob // set by --join; mutually exclusive with --session
	Name           string
	Resume         bool // --resume: fail loudly instead of silently registering fresh if no saved identity resumes
	Fresh          bool // --fresh: ignore any saved identity and register brand new on purpose
	ApproveInbound bool
	Record         Record
	ToolArgs       []string

	// admin
	All        bool   // ls --all
	Target     string // ls --session / session end|invite|peers <id>
	SessionNm  string // session new --name
	Host       bool   // session new --host: also mint a --join invite
	Foreground bool   // daemon --foreground

	// messaging commands
	Sub      string   // approve: "" | ls | accept | reject
	Words    []string // positional arguments
	Priority string   // send --priority
	MsgKind  string   // send --kind
	Agent    string   // messages --agent
	State    string   // messages --state
	Limit    int      // messages --limit

	// gc
	OlderThan time.Duration // gc --older-than
	Compress  bool          // gc --compress
	DryRun    bool          // gc --dry-run
}

// UsageError is a mistake in how relay was invoked (exit status 2).
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func usagef(format string, a ...any) error { return &UsageError{fmt.Sprintf(format, a...)} }

// Parse interprets argv (including argv[0]).
func Parse(argv []string, f *adaptor.AdaptorFactory) (Parsed, error) {
	// Shim mode: a symlink named "claude" or "codex". Everything belongs to the tool.
	if _, ok := f.ByName(filepath.Base(argv[0])); ok {
		return Parsed{Kind: KindAgent, Tool: filepath.Base(argv[0]), Shim: true, Record: RecordRaw, ToolArgs: argv[1:]}, nil
	}
	if len(argv) < 2 {
		return Parsed{Kind: KindHelp}, nil
	}
	first, rest := argv[1], argv[2:]
	switch first {
	case "-h", "--help", "help":
		return Parsed{Kind: KindHelp}, nil
	case "-V", "--version", "version":
		return Parsed{Kind: KindVersion}, nil
	case "ls":
		return parseLs(rest)
	case "session":
		return parseSession(rest)
	case "daemon":
		return parseDaemon(rest)
	case "mcp":
		return parseMCP(rest)
	case "hook":
		return parseHook(rest)
	case "doctor":
		if len(rest) != 0 {
			return Parsed{}, usagef("relay doctor takes no arguments")
		}
		return Parsed{Kind: KindDoctor}, nil
	case "approve":
		return parseApprove(rest)
	case "send":
		return parseSend(rest)
	case "messages", "msgs":
		return parseMessages(rest)
	case "gc":
		return parseGC(rest)
	}
	if _, ok := f.ByName(first); ok {
		return parseAgent(first, rest)
	}
	return Parsed{}, usagef("unknown command or tool %q (tools: %s)", first, strings.Join(f.Names(), ", "))
}

func parseAgent(tool string, args []string) (Parsed, error) {
	p := Parsed{Kind: KindAgent, Tool: strings.ToLower(tool), Record: RecordRaw}
	var haveRole, haveSession, haveName, haveJoin bool
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.ToolArgs = args[i+1:]
			break
		}
		if !strings.HasPrefix(a, "-") {
			if haveRole {
				return p, usagef("unexpected argument %q (tool arguments go after --)", a)
			}
			p.Role, haveRole = a, true
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		// value flags accept "--flag value" too
		need := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) || args[i+1] == "--" {
				return "", usagef("--%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch name {
		case "session":
			v, err := need()
			if err != nil {
				return p, err
			}
			if haveSession {
				return p, usagef("--session given twice")
			}
			if haveJoin {
				return p, usagef("--session and --join are mutually exclusive: --join already names a session")
			}
			haveSession = true
			switch {
			case strings.EqualFold(v, proto.SessionNew):
				p.Session = proto.SessionNew
			case ids.Valid(v):
				p.Session = ids.Normalize(v)
			default:
				return p, usagef("--session must be NEW or a session ID (a 26-character ULID), got %q", v)
			}
		case "join":
			v, err := need()
			if err != nil {
				return p, err
			}
			if haveJoin {
				return p, usagef("--join given twice")
			}
			if haveSession {
				return p, usagef("--session and --join are mutually exclusive: --join already names a session")
			}
			haveJoin = true
			blob, err := proto.ParseJoinBlob(v)
			if err != nil {
				return p, usagef("--join: %v", err)
			}
			p.Join = &blob
		case "name":
			v, err := need()
			if err != nil {
				return p, err
			}
			if haveName {
				return p, usagef("--name given twice")
			}
			haveName = true
			if ok, why := naming.Validate(v); !ok {
				return p, usagef("--name %q: %s", v, why)
			}
			p.Name = v
		case "role":
			v, err := need()
			if err != nil {
				return p, err
			}
			if haveRole {
				return p, usagef("role given twice (positional and --role)")
			}
			p.Role, haveRole = v, true
		case "resume":
			p.Resume = true
		case "fresh":
			p.Fresh = true
		case "approve-inbound":
			p.ApproveInbound = true
			if hasVal {
				switch strings.ToLower(val) {
				case "true", "1", "yes":
				case "false", "0", "no":
					p.ApproveInbound = false
				default:
					return p, usagef("--approve-inbound expects true or false, got %q", val)
				}
			}
		case "record":
			v, err := need()
			if err != nil {
				return p, err
			}
			switch r := Record(strings.ToLower(v)); r {
			case RecordRaw, RecordEvents, RecordOff:
				p.Record = r
			default:
				return p, usagef("--record must be raw, events or off, got %q", v)
			}
		case "h", "help":
			return Parsed{Kind: KindHelp}, nil
		default:
			return p, usagef("unknown option %q: relay options go before --, tool options after it (relay %s ... -- %s)", a, tool, a)
		}
	}
	if p.ApproveInbound && p.Session == "" && p.Join == nil {
		return p, usagef("--approve-inbound only makes sense with --session or --join (there is no one to receive messages from)")
	}
	if p.Name != "" && p.Session == "" && p.Join == nil {
		return p, usagef("--name only makes sense with --session or --join")
	}
	if p.Resume && p.Fresh {
		return p, usagef("--resume and --fresh are mutually exclusive")
	}
	if (p.Resume || p.Fresh) && p.Name == "" {
		return p, usagef("--resume and --fresh only make sense with --name (there is no saved identity to act on without one)")
	}
	return p, nil
}

func parseLs(args []string) (Parsed, error) {
	p := Parsed{Kind: KindLs}
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch name {
		case "all", "a":
			p.All = true
		case "session":
			if !hasVal {
				if i+1 >= len(args) {
					return p, usagef("--session needs a value")
				}
				i++
				val = args[i]
			}
			if !ids.Valid(val) {
				return p, usagef("--session must be a session ID, got %q", val)
			}
			p.Target = ids.Normalize(val)
		default:
			return p, usagef("unknown option %q for ls (try --all or --session=<id>)", args[i])
		}
	}
	return p, nil
}

func parseSession(args []string) (Parsed, error) {
	if len(args) == 0 {
		return Parsed{}, usagef("usage: relay session new [--name=<label>] [--host] | ls | end <id> | invite <id> | peers <id>")
	}
	switch args[0] {
	case "ls", "list":
		return parseLs(args[1:])
	case "new":
		p := Parsed{Kind: KindSessionNew}
		for i := 1; i < len(args); i++ {
			name, val, hasVal := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
			switch name {
			case "name":
				if !hasVal {
					if i+1 >= len(args) {
						return p, usagef("--name needs a value")
					}
					i++
					val = args[i]
				}
				p.SessionNm = val
			case "host":
				p.Host = true
			default:
				return p, usagef("unknown option %q for session new (try --name=<label> or --host)", args[i])
			}
		}
		return p, nil
	case "end":
		if len(args) != 2 || !ids.Valid(args[1]) {
			return Parsed{}, usagef("usage: relay session end <session-id>")
		}
		return Parsed{Kind: KindSessionEnd, Target: ids.Normalize(args[1])}, nil
	case "invite":
		if len(args) != 2 || !ids.Valid(args[1]) {
			return Parsed{}, usagef("usage: relay session invite <session-id>")
		}
		return Parsed{Kind: KindSessionInvite, Target: ids.Normalize(args[1])}, nil
	case "peers":
		if len(args) != 2 || !ids.Valid(args[1]) {
			return Parsed{}, usagef("usage: relay session peers <session-id>")
		}
		return Parsed{Kind: KindSessionPeers, Target: ids.Normalize(args[1])}, nil
	}
	return Parsed{}, usagef("unknown session command %q (new, ls, end, invite, peers)", args[0])
}

func parseDaemon(args []string) (Parsed, error) {
	p := Parsed{Kind: KindDaemon}
	for _, a := range args {
		switch a {
		case "stop":
			p.Kind = KindDaemonStop
		case "status":
			p.Kind = KindDaemonStatus
		case "-f", "--foreground":
			p.Foreground = true
		default:
			return p, usagef("unknown daemon option %q (stop, status, --foreground)", a)
		}
	}
	return p, nil
}

// flagValue reads "--name=value" or "--name value" at args[i]; it returns the
// value and the index of the last argument consumed.
func flagValue(args []string, i int, name string) (string, int, error) {
	if _, v, ok := strings.Cut(args[i], "="); ok {
		return v, i, nil
	}
	if i+1 >= len(args) {
		return "", i, usagef("--%s needs a value", name)
	}
	return args[i+1], i + 1, nil
}

func sessionFlag(v string) (string, error) {
	if !ids.Valid(v) {
		return "", usagef("--session must be a session ID, got %q", v)
	}
	return ids.Normalize(v), nil
}

func parseMCP(args []string) (Parsed, error) {
	p := Parsed{Kind: KindMCP}
	for i := 0; i < len(args); i++ {
		name := strings.TrimLeft(strings.SplitN(args[i], "=", 2)[0], "-")
		if name != "dir" {
			return p, usagef("unknown option %q for mcp (usage: relay mcp --dir <run dir>)", args[i])
		}
		v, j, err := flagValue(args, i, "dir")
		if err != nil {
			return p, err
		}
		p.Target, i = v, j
	}
	if p.Target == "" {
		return p, usagef("usage: relay mcp --dir <run dir> (started by the tool, not by hand)")
	}
	return p, nil
}

func parseApprove(args []string) (Parsed, error) {
	p := Parsed{Kind: KindApprove}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
			if name != "session" {
				return p, usagef("unknown option %q for approve (try --session=<id>)", a)
			}
			v, j, err := flagValue(args, i, "session")
			if err != nil {
				return p, err
			}
			if p.Target, err = sessionFlag(v); err != nil {
				return p, err
			}
			i = j
			continue
		}
		if p.Sub == "" {
			switch a {
			case "ls", "list", "accept", "approve", "reject", "deny":
				p.Sub = map[string]string{"list": "ls", "approve": "accept", "deny": "reject"}[a]
				if p.Sub == "" {
					p.Sub = a
				}
				continue
			}
		}
		p.Words = append(p.Words, a)
	}
	switch p.Sub {
	case "accept", "reject":
		if len(p.Words) != 1 {
			return p, usagef("usage: relay approve %s <message-id|all>", p.Sub)
		}
	case "ls", "":
		if len(p.Words) != 0 {
			return p, usagef("unexpected argument %q (usage: relay approve [ls|accept|reject] [message-id|all])", p.Words[0])
		}
	}
	return p, nil
}

func parseSend(args []string) (Parsed, error) {
	p := Parsed{Kind: KindSend}
	rest := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if rest || !strings.HasPrefix(a, "-") || a == "-" {
			p.Words = append(p.Words, a)
			continue
		}
		if a == "--" {
			rest = true
			continue
		}
		name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
		v, j, err := flagValue(args, i, name)
		switch name {
		case "session":
			if err != nil {
				return p, err
			}
			if p.Target, err = sessionFlag(v); err != nil {
				return p, err
			}
		case "priority", "p":
			if err != nil {
				return p, err
			}
			if _, perr := proto.ParsePriority(v); perr != nil {
				return p, usagef("%v", perr)
			}
			p.Priority = v
		case "kind":
			if err != nil {
				return p, err
			}
			if !proto.ValidKind(v) {
				return p, usagef("--kind must be one of %s", strings.Join(proto.Kinds, ", "))
			}
			p.MsgKind = v
		default:
			return p, usagef("unknown option %q for send (--session, --priority, --kind)", a)
		}
		i = j
	}
	if len(p.Words) < 2 {
		return p, usagef("usage: relay send <agent> <text...>   (use - as the text to read it from stdin)")
	}
	return p, nil
}

func parseMessages(args []string) (Parsed, error) {
	p := Parsed{Kind: KindMessages, Limit: 30}
	for i := 0; i < len(args); i++ {
		name := strings.TrimLeft(strings.SplitN(args[i], "=", 2)[0], "-")
		v, j, err := flagValue(args, i, name)
		switch name {
		case "session":
			if err != nil {
				return p, err
			}
			if p.Target, err = sessionFlag(v); err != nil {
				return p, err
			}
		case "agent":
			if err != nil {
				return p, err
			}
			p.Agent = v
		case "state":
			if err != nil {
				return p, err
			}
			p.State = v
		case "limit", "n":
			if err != nil {
				return p, err
			}
			n, cerr := strconv.Atoi(v)
			if cerr != nil || n < 1 || n > 1000 {
				return p, usagef("--limit must be 1-1000")
			}
			p.Limit = n
		default:
			return p, usagef("unknown option %q for messages (--session, --agent, --state, --limit)", args[i])
		}
		i = j
	}
	if p.Agent != "" && p.Target == "" {
		return p, usagef("--agent needs --session")
	}
	return p, nil
}

func parseHook(args []string) (Parsed, error) {
	p := Parsed{Kind: KindHook}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-") != "dir" {
				return p, usagef("unknown option %q for hook (usage: relay hook <event> --dir <run dir>)", a)
			}
			v, j, err := flagValue(args, i, "dir")
			if err != nil {
				return p, err
			}
			p.Target, i = v, j
			continue
		}
		p.Sub = a // the event name (also present in the payload)
	}
	if p.Target == "" {
		return p, usagef("usage: relay hook <event> --dir <run dir> (started by Claude Code, not by hand)")
	}
	return p, nil
}

// ParseAge reads "90m", "12h", "30d" or "2w".
func ParseAge(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return 0, fmt.Errorf("empty duration")
	}
	unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	mult, ok := unit[v[len(v)-1]]
	if !ok {
		return 0, fmt.Errorf("%q: use a number with m, h, d or w (e.g. 30d)", v)
	}
	n, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q: use a positive number with m, h, d or w (e.g. 30d)", v)
	}
	return time.Duration(n) * mult, nil
}

func parseGC(args []string) (Parsed, error) {
	p := Parsed{Kind: KindGC}
	for i := 0; i < len(args); i++ {
		name := strings.TrimLeft(strings.SplitN(args[i], "=", 2)[0], "-")
		switch name {
		case "dry-run":
			p.DryRun = true
		case "compress":
			p.Compress = true
		case "older-than":
			v, j, err := flagValue(args, i, name)
			if err != nil {
				return p, err
			}
			if p.OlderThan, err = ParseAge(v); err != nil {
				return p, usagef("--older-than %v", err)
			}
			i = j
		default:
			return p, usagef("unknown option %q for gc (--older-than=<30d>, --compress, --dry-run)", args[i])
		}
	}
	if p.DryRun && p.OlderThan == 0 && !p.Compress {
		return p, usagef("--dry-run needs --older-than and/or --compress")
	}
	return p, nil
}
