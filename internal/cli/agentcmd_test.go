package cli

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

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

// TestShouldPauseForBannerAck_FalseForNonFileReader proves a plain
// (non-*os.File) reader - what every test harness that doesn't spawn a real
// terminal uses, and what a pipe/`< /dev/null` looks like in practice - never
// triggers the pause. This is the guard that keeps issue #41's fix from
// hanging any non-interactive caller.
func TestShouldPauseForBannerAck_FalseForNonFileReader(t *testing.T) {
	if shouldPauseForBannerAck(strings.NewReader("")) {
		t.Error("a non-*os.File reader must never trigger the pause")
	}
}

// TestShouldPauseForBannerAck_TrueForARealTerminalFalseOtherwise uses a real
// PTY (the same mechanism internal/agent.Run and the e2e test harness use) to
// prove both branches for real: a genuine terminal triggers the pause, and
// the $RELAY_SKIP_SESSION_PROMPT escape hatch suppresses it even then -
// exercising the actual isatty check, not just the reader-type guard above.
func TestShouldPauseForBannerAck_TrueForARealTerminalFalseOtherwise(t *testing.T) {
	_, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no PTY available in this environment: %v", err)
	}
	defer tty.Close()

	if !shouldPauseForBannerAck(tty) {
		t.Error("a real terminal should trigger the pause")
	}

	t.Setenv(sessionBannerAckEnvVar, "1")
	if shouldPauseForBannerAck(tty) {
		t.Error("RELAY_SKIP_SESSION_PROMPT should suppress the pause even on a real terminal")
	}
}

// TestWaitForBannerAck_ReturnsAsSoonAsEnterIsPressed proves the block is
// genuinely lifted by input, not by a timer - and that whatever was typed
// before Enter is simply discarded, not interpreted.
func TestWaitForBannerAck_ReturnsAsSoonAsEnterIsPressed(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()

	done := make(chan struct{})
	go func() {
		waitForBannerAck(r, io.Discard)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("waitForBannerAck returned before anything was written")
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := w.Write([]byte("anything at all\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForBannerAck did not return after Enter was pressed")
	}
}
