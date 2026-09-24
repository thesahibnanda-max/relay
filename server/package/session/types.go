package session

import "github.com/thesahibnanda-max/relay/server/package/database/mongodb"

// SendRequest is what a send RPC carries once decoded off the wire.
type SendRequest struct {
	To, Body, Kind, ReplyTo string
	// Priority is the already-resolved priority to store (mongodb.DefaultMessagePriority
	// if the caller has no opinion). Parsing the wire's string encoding
	// ("p0"/"high"/"normal"/...) and filling in that default happens in
	// ws.handleSend, not here - 0 is a legitimate priority value (P0/
	// interrupt) and can't double as an "unset" sentinel the way an empty
	// Kind string can.
	Priority int
}

// SendOutcome is what a successful Send reports back - including the
// resolved Kind/Priority (after defaulting) so a caller building a wire
// MessageView doesn't need to duplicate Send's own defaulting logic.
type SendOutcome struct {
	MessageID, TargetAgentID, State, Kind string
	Priority                              int
	// Note explains a non-obvious outcome - e.g. "duplicate of a recent
	// identical message" when Send coalesced into an existing message
	// instead of creating a new one. Empty in the ordinary case.
	Note string
}

// WaitOutcome is what Wait reports once it stops polling.
type WaitOutcome struct {
	State    string
	Reply    *mongodb.Message
	TimedOut bool
}
