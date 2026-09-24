package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// savedIdentity is what connect persists after every successful Welcome for
// a named agent in an explicit session, so a fresh process (the previous
// one crashed, or was killed and relaunched with the exact same command)
// can resume as the same agent instead of colliding on its still-occupied
// name. A flat file, not SQLite: the CLI never opens store.Store directly
// (only the daemon does), and routing this through a new RPC just to read
// back the CLI's own token would add a round trip for no benefit.
type savedIdentity struct {
	V       int       `json:"v"`
	Session string    `json:"session"`
	Name    string    `json:"name"`
	Token   string    `json:"token"`
	Tool    string    `json:"tool"`
	SavedAt time.Time `json:"saved_at"`
}

const identityFileVersion = 1

// identityPath is deterministic from (session, name) alone, lowercased
// since names are matched case-insensitively - so "Bob" and "bob" always
// resolve to the same saved identity.
func identityPath(paths relayhome.Paths, sessionID, name string) string {
	return filepath.Join(paths.IdentitiesDir(), sessionID+"__"+strings.ToLower(name)+".json")
}

// globalIdentityKey makes a global session's shareable token
// ("<ulid>@host:port") safe to use as (part of) identityPath's filename -
// ':' is reserved in a Windows filename. A bare local ULID never contains
// ':', so this is a no-op for local sessions - callers may apply it
// unconditionally. Equivalent to globalid.Token.FileSafe() for callers that
// already have a parsed Token rather than the plain string.
func globalIdentityKey(sessionID string) string {
	return strings.ReplaceAll(sessionID, ":", "%3A")
}

// saveIdentity writes (session, name)'s resume token, atomically (a temp
// file then a rename) so a crash mid-write never leaves the next process a
// corrupt file to trip over. Best-effort throughout: resume is a
// convenience, never a hard requirement, so a write failure here must never
// fail the caller's connection that just succeeded.
func saveIdentity(paths relayhome.Paths, sessionID, name, token, tool string) {
	if token == "" || sessionID == "" || name == "" {
		return
	}
	if err := os.MkdirAll(paths.IdentitiesDir(), 0o700); err != nil {
		return
	}
	b, err := json.Marshal(savedIdentity{V: identityFileVersion, Session: sessionID, Name: name, Token: token, Tool: tool, SavedAt: time.Now()})
	if err != nil {
		return
	}
	dst := identityPath(paths, sessionID, name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, dst)
}

// loadIdentity reads back a previously saved identity, if any. A missing,
// corrupt or empty-token file is never an error the caller need react to
// beyond "there is nothing to resume."
func loadIdentity(paths relayhome.Paths, sessionID, name string) (savedIdentity, bool) {
	b, err := os.ReadFile(identityPath(paths, sessionID, name))
	if err != nil {
		return savedIdentity{}, false
	}
	var id savedIdentity
	if json.Unmarshal(b, &id) != nil || id.Token == "" {
		return savedIdentity{}, false
	}
	return id, true
}

// deleteIdentity removes a stale saved identity (its token was rejected, or
// the session it names is gone) so a later invocation doesn't keep
// retrying a resume that will never succeed.
func deleteIdentity(paths relayhome.Paths, sessionID, name string) {
	_ = os.Remove(identityPath(paths, sessionID, name))
}
