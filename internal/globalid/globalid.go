// Package globalid parses and formats global (multi-machine) session
// tokens: "<ULID>@<host>[:<port>]", embedding the central server's address
// directly in the shareable session identifier so a teammate on a different
// machine can join with one copy-pasted string and nothing else to
// configure. A bare 26-char ULID (no '@') keeps meaning "local session" -
// full backward compatibility, and unambiguous by construction: a valid
// ULID's alphabet never contains '@'.
package globalid

import (
	"net"
	"strconv"
	"strings"

	"github.com/thesahibnanda-max/relay/internal/ids"
)

// DefaultPort mirrors server/package/config.Config's PORT default -
// duplicated on purpose, since the root module can never import server/ (a
// separate Go module). Keep the two in sync by hand if either changes.
const DefaultPort = 5555

// Token identifies a global session: which ULID the server knows it as, and
// which server address to dial to reach it.
type Token struct {
	ULID     string
	HostPort string // always "host:port" - the port is never left implicit
}

// String returns the shareable form: "<ULID>@<host:port>".
func (t Token) String() string {
	return t.ULID + "@" + t.HostPort
}

// FileSafe returns String() with ':' replaced, so it's safe to use as (part
// of) a filename on every platform this repo supports, including Windows -
// used for a global session's resume-identity file name.
func (t Token) FileSafe() string {
	return strings.ReplaceAll(t.String(), ":", "%3A")
}

// Parse reports ok=false unless s is exactly "<26-char-ULID>@<host>[:<port>]".
// Callers must check ids.Valid(s) first for the bare-local-ULID case; Parse
// only ever needs to handle the "contains an '@'" case.
func Parse(s string) (Token, bool) {
	s = strings.TrimSpace(s) // tmux/clipboard paste hygiene
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return Token{}, false
	}
	ulidPart, hostPart := s[:at], s[at+1:]
	if !ids.Valid(ulidPart) {
		return Token{}, false
	}
	hostPort, ok := normalizeHostPort(hostPart)
	if !ok {
		return Token{}, false
	}
	return Token{ULID: ids.Normalize(ulidPart), HostPort: hostPort}, true
}

// normalizeHostPort fills in DefaultPort when hostPart names no port, and
// rejects anything it can't confidently parse rather than guess - a
// malformed address that slips through is caught for real the moment it's
// actually dialed.
func normalizeHostPort(hostPart string) (string, bool) {
	if hostPart == "" {
		return "", false
	}
	if host, port, err := net.SplitHostPort(hostPart); err == nil {
		if host == "" || port == "" {
			return "", false
		}
		if _, err := strconv.Atoi(port); err != nil {
			return "", false
		}
		return net.JoinHostPort(host, port), true
	}

	// No port given. A bracketed IPv6 literal needs its brackets stripped
	// before rejoining, since JoinHostPort re-adds them itself for any host
	// containing a colon.
	host := hostPart
	switch {
	case strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]"):
		host = host[1 : len(host)-1]
	case strings.Contains(host, ":"):
		// An unbracketed host containing a colon (e.g. a bare IPv6 literal
		// "::1") is ambiguous without brackets - reject rather than guess.
		return "", false
	}
	if host == "" {
		return "", false
	}
	return net.JoinHostPort(host, strconv.Itoa(DefaultPort)), true
}
