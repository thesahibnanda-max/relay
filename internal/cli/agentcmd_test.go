package cli

import (
	"strings"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/globalid"
	"github.com/thesahibnanda-max/relay/internal/proto"
)

// TestGlobalDialTarget_LocalBuildNewSessionUsesServerFlag proves a build with
// no builtin server configured (today's exact behavior) still requires
// --server/RELAY_SERVER for a brand new global session, unchanged.
func TestGlobalDialTarget_LocalBuildNewSessionUsesServerFlag(t *testing.T) {
	setBuiltinServerURL(t, "")
	p := Parsed{SessionKind: SessionKindGlobalNew, Server: "example.com:5555"}
	hostPort, sessionValue, forceTLS, err := globalDialTarget(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostPort != "example.com:5555" || sessionValue != proto.SessionNew || forceTLS {
		t.Errorf("got hostPort=%q sessionValue=%q forceTLS=%v", hostPort, sessionValue, forceTLS)
	}
}

// TestGlobalDialTarget_LocalBuildNewSessionWithNoServerFailsClearly proves
// the missing --server/RELAY_SERVER error text is unchanged for a
// local/from-source build.
func TestGlobalDialTarget_LocalBuildNewSessionWithNoServerFailsClearly(t *testing.T) {
	setBuiltinServerURL(t, "")
	p := Parsed{SessionKind: SessionKindGlobalNew}
	_, _, _, err := globalDialTarget(p)
	if err == nil || !strings.Contains(err.Error(), "--session=NEW needs a server") {
		t.Fatalf("expected the standard missing-server error, got %v", err)
	}
}

// TestGlobalDialTarget_LocalBuildJoinUsesTokenHost proves a join token's own
// host is used as-is when no builtin server is configured.
func TestGlobalDialTarget_LocalBuildJoinUsesTokenHost(t *testing.T) {
	setBuiltinServerURL(t, "")
	tok := globalid.Token{ULID: "01TESTSESSIONULID0000000A", HostPort: "example.com:5555"}
	p := Parsed{SessionKind: SessionKindGlobalJoin, GlobalToken: tok}
	hostPort, sessionValue, forceTLS, err := globalDialTarget(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostPort != "example.com:5555" || sessionValue != tok.ULID || forceTLS {
		t.Errorf("got hostPort=%q sessionValue=%q forceTLS=%v", hostPort, sessionValue, forceTLS)
	}
}

// TestGlobalDialTarget_ReleaseBuildNewSessionUsesBuiltinServer proves an
// official release binary (builtinServerURL set) needs no --server at all -
// it always uses its one baked-in server, with TLS forced.
func TestGlobalDialTarget_ReleaseBuildNewSessionUsesBuiltinServer(t *testing.T) {
	setBuiltinServerURL(t, "relay.example.com:443")
	p := Parsed{SessionKind: SessionKindGlobalNew}
	hostPort, sessionValue, forceTLS, err := globalDialTarget(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostPort != "relay.example.com:443" || sessionValue != proto.SessionNew || !forceTLS {
		t.Errorf("got hostPort=%q sessionValue=%q forceTLS=%v", hostPort, sessionValue, forceTLS)
	}
}

// TestGlobalDialTarget_ReleaseBuildRejectsExplicitOtherServer proves
// --server/RELAY_SERVER naming anything but the builtin server is a clear,
// rejected error in an official release binary.
func TestGlobalDialTarget_ReleaseBuildRejectsExplicitOtherServer(t *testing.T) {
	setBuiltinServerURL(t, "relay.example.com:443")
	p := Parsed{SessionKind: SessionKindGlobalNew, Server: "someone-elses-server.com:5555"}
	_, _, _, err := globalDialTarget(p)
	if err == nil {
		t.Fatal("expected an error rejecting the non-builtin server")
	}
}

// TestGlobalDialTarget_ReleaseBuildRejectsJoinTokenNamingOtherHost proves the
// second loophole (a join token's own embedded host) is closed exactly like
// --server is: an official release binary refuses to dial a token naming any
// host but its one builtin server.
func TestGlobalDialTarget_ReleaseBuildRejectsJoinTokenNamingOtherHost(t *testing.T) {
	setBuiltinServerURL(t, "relay.example.com:443")
	tok := globalid.Token{ULID: "01TESTSESSIONULID0000000A", HostPort: "someone-elses-server.com:5555"}
	p := Parsed{SessionKind: SessionKindGlobalJoin, GlobalToken: tok}
	_, _, _, err := globalDialTarget(p)
	if err == nil {
		t.Fatal("expected an error rejecting the join token's non-builtin host")
	}
}

// TestGlobalDialTarget_ReleaseBuildJoinTokenNamingBuiltinHostSucceeds proves
// a join token naming exactly the builtin server still works.
func TestGlobalDialTarget_ReleaseBuildJoinTokenNamingBuiltinHostSucceeds(t *testing.T) {
	setBuiltinServerURL(t, "relay.example.com:443")
	tok := globalid.Token{ULID: "01TESTSESSIONULID0000000A", HostPort: "relay.example.com:443"}
	p := Parsed{SessionKind: SessionKindGlobalJoin, GlobalToken: tok}
	hostPort, sessionValue, forceTLS, err := globalDialTarget(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostPort != "relay.example.com:443" || sessionValue != tok.ULID || !forceTLS {
		t.Errorf("got hostPort=%q sessionValue=%q forceTLS=%v", hostPort, sessionValue, forceTLS)
	}
}
