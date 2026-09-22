package proto

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thesahibnanda-max/relay/internal/ids"
)

// JoinBlobPrefix marks a --join value as a Relay join blob, so Parse and
// error messages can say what went wrong before attempting to decode.
const JoinBlobPrefix = "relay-join-v1."

// JoinBlobVersion is the join blob's own encoding version, independent of
// MeshVersion: a future encoding change can be rejected with a clear error
// instead of silently misparsing.
const JoinBlobVersion = 1

// JoinBlob is what one mesh member's "relay session invite" prints and
// another machine's --join=<blob> pastes back in. It is a bearer credential
// end to end - anyone holding it can join the named session - and is
// documented as such: never paste one into a public channel.
type JoinBlob struct {
	V        int    `json:"v"`
	Session  string `json:"session"`             // session ULID
	PeerAddr string `json:"peer_addr"`           // minting daemon's meshnet.Addr
	PeerName string `json:"peer_name,omitempty"` // optional hostname label, shown in prompts
	Secret   string `json:"secret"`              // the session's join secret, in the clear
}

// EncodeJoinBlob renders b as the opaque string a user copies and pastes.
func EncodeJoinBlob(b JoinBlob) string {
	b.V = JoinBlobVersion
	data, err := json.Marshal(b)
	if err != nil {
		// Every field is a plain string; this cannot fail in practice.
		panic(fmt.Sprintf("proto: encoding join blob: %v", err))
	}
	return JoinBlobPrefix + base64.RawURLEncoding.EncodeToString(data)
}

// ParseJoinBlob decodes and validates a --join value, failing fast and
// clearly here rather than letting a malformed blob reach the daemon and
// surface as a more confusing failure deep in the mesh join RPC.
func ParseJoinBlob(s string) (JoinBlob, error) {
	rest, ok := strings.CutPrefix(s, JoinBlobPrefix)
	if !ok {
		return JoinBlob{}, fmt.Errorf("that doesn't look like a relay join blob (expected it to start with %q)", JoinBlobPrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return JoinBlob{}, fmt.Errorf("invalid join blob: %w", err)
	}
	var b JoinBlob
	if err := json.Unmarshal(data, &b); err != nil {
		return JoinBlob{}, fmt.Errorf("invalid join blob: %w", err)
	}
	if b.V != JoinBlobVersion {
		return JoinBlob{}, fmt.Errorf("this join blob is v%d, but this build of relay only understands v%d", b.V, JoinBlobVersion)
	}
	if !ids.Valid(b.Session) {
		return JoinBlob{}, fmt.Errorf("invalid join blob: bad session id")
	}
	if b.PeerAddr == "" {
		return JoinBlob{}, fmt.Errorf("invalid join blob: missing peer address")
	}
	if b.Secret == "" {
		return JoinBlob{}, fmt.Errorf("invalid join blob: missing secret")
	}
	b.Session = ids.Normalize(b.Session)
	return b, nil
}
