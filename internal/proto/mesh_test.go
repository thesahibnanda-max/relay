package proto

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestMeshMarshalRoundTrips(t *testing.T) {
	hello := MeshHello{
		MeshVersion: MeshVersion,
		Session:     "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Secret:      "s3cr3t",
		PeerID:      "abc123",
		Addr:        "tcXXXXXXXXX",
		DaemonBuild: "dev",
		Agents:      []MeshAgentInfo{{AgentID: "01AG", Name: "coder", Version: 1}},
	}
	data, err := MeshMarshal(MeshTypeHello, hello)
	if err != nil {
		t.Fatal(err)
	}
	env, err := MeshUnmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if env.V != MeshVersion || env.Type != MeshTypeHello {
		t.Fatalf("envelope %+v", env)
	}
	var got MeshHello
	if err := json.Unmarshal(env.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, hello) {
		t.Fatalf("round trip changed the payload: %+v vs %+v", got, hello)
	}
}

func TestMeshEnvelopeIsDistinctFromTheAgentEnvelope(t *testing.T) {
	// An agent's Envelope and a mesh link's MeshEnvelope must never be
	// interchangeable: this is what stops a frame meant for one protocol
	// from being silently misread as the other.
	agentData, err := Marshal(TypeHello, Hello{Proto: Version, Tool: "claude", Role: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	var asEnv Envelope
	if err := json.Unmarshal(agentData, &asEnv); err != nil {
		t.Fatal(err)
	}
	if asEnv.V != Version {
		t.Fatalf("agent envelope carries the agent Version, got %d", asEnv.V)
	}

	meshData, err := MeshMarshal(MeshTypeHello, MeshHello{MeshVersion: MeshVersion, Session: "x", Secret: "y"})
	if err != nil {
		t.Fatal(err)
	}
	var asMeshEnv MeshEnvelope
	if err := json.Unmarshal(meshData, &asMeshEnv); err != nil {
		t.Fatal(err)
	}
	if asMeshEnv.V != MeshVersion {
		t.Fatalf("mesh envelope carries MeshVersion, got %d", asMeshEnv.V)
	}
	if Version == MeshVersion {
		t.Fatal("Version and MeshVersion happen to collide; the two protocols must stay independently versioned")
	}
}

func TestMeshUnmarshalRejectsAMissingType(t *testing.T) {
	if _, err := MeshUnmarshal([]byte(`{"v":1,"payload":{}}`)); err == nil {
		t.Fatal("expected an error for a mesh envelope with no type")
	}
}

func TestMeshWelcomeCarriesAFullResync(t *testing.T) {
	w := MeshWelcome{
		PeerID: "self",
		Addr:   "tcSELF",
		Peers:  []MeshPeerInfo{{PeerID: "other", Addr: "tcOTHER", LastSeen: time.Now().UTC()}},
		Agents: []MeshAgentInfo{{AgentID: "01AG", OwnerPeer: "other", Name: "coder", Tool: "codex", Version: 1}},
	}
	data, err := MeshMarshal(MeshTypeWelcome, w)
	if err != nil {
		t.Fatal(err)
	}
	env, err := MeshUnmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var got MeshWelcome
	if err := json.Unmarshal(env.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Peers) != 1 || len(got.Agents) != 1 {
		t.Fatalf("welcome lost data in a round trip: %+v", got)
	}
	if !got.Peers[0].LastSeen.Equal(w.Peers[0].LastSeen) {
		t.Fatalf("timestamp changed: %v vs %v", got.Peers[0].LastSeen, w.Peers[0].LastSeen)
	}
}

// FuzzMeshEnvelope: a daemon decodes whatever a mesh peer sends over the
// tunnel. Nothing may panic, mirroring FuzzEnvelope's contract for the
// agent-facing protocol.
func FuzzMeshEnvelope(f *testing.F) {
	for _, seed := range []string{
		`{"v":1,"type":"mesh_hello","payload":{"mesh_version":1,"session":"x","secret":"y"}}`,
		`{"v":1,"type":"mesh_msg","payload":{"id":"01ARZ3","hops":8,"priority":1}}`,
		`{"v":1,"type":"mesh_agent_roster","payload":{"agent_id":"a","version":18446744073709551615}}`,
		`{}`, `[]`, `null`, `{"type":"x","payload":`, "\x00",
		`{"type":"mesh_msg_receipt","payload":{"rev":-1}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		env, err := MeshUnmarshal(data)
		if err != nil {
			return
		}
		var hello MeshHello
		var welcome MeshWelcome
		var handoff MeshMsgHandoff
		var receipt MeshMsgReceipt
		var ack MeshMsgAck
		var agent MeshAgentInfo
		_ = json.Unmarshal(env.Payload, &hello)
		_ = json.Unmarshal(env.Payload, &welcome)
		_ = json.Unmarshal(env.Payload, &handoff)
		_ = json.Unmarshal(env.Payload, &receipt)
		_ = json.Unmarshal(env.Payload, &ack)
		_ = json.Unmarshal(env.Payload, &agent)
		if b, err := MeshMarshal(env.Type, env.Payload); err == nil && len(b) == 0 {
			t.Fatal("empty marshal")
		}
	})
}
