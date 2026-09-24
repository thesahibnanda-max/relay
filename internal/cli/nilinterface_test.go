package cli

import (
	"testing"

	"github.com/thesahibnanda-max/relay/internal/adaptor"
	"github.com/thesahibnanda-max/relay/internal/collab"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/roles"
)

// TestConnectFailureReturnsATrueNilLink guards the exact trap introduced by
// switching runAgent's lk from a concrete *link.Client (whose methods were
// nil-receiver-safe) to the collab.Link interface (whose methods panic on a
// true nil receiver - there's no concrete type to dispatch to). connect,
// connectLocal and connectGlobal must only ever return a literal nil on
// failure, never a typed-nil pointer boxed into the interface, or every
// unconditional lk.Identity()/lk.Close() call site in agentcmd.go would
// panic on solo-mode-like failures instead of degrading gracefully.
func TestConnectFailureReturnsATrueNilLink(t *testing.T) {
	paths := newResumeTestDaemon(t) // a real local daemon, but naming a session that doesn't exist
	factory := adaptor.NewAdaptorFactory()
	a, _ := factory.ByName("claude")
	role, err := roles.Resolve("developer")
	if err != nil {
		t.Fatal(err)
	}
	col := collab.New()

	p := Parsed{SessionKind: SessionKindLocalJoin, Session: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "nemo"}
	lk, err := connect(paths, p, role, a, col)
	if err == nil {
		t.Fatal("expected connect to fail: that session does not exist")
	}
	// The critical assertion: comparing an interface value to the nil
	// literal is false whenever it holds ANY concrete type, even a nil
	// pointer of that type - so this only passes for a genuinely nil
	// interface, exactly what a solo-mode caller needs to skip every
	// lk.X(...) call safely.
	if lk != nil {
		t.Fatalf("expected connect's failure return to be a true nil collab.Link, got a non-nil interface: %#v", lk)
	}
}

// TestConnectSuccessReturnsAUsableLink is the mirror-image sanity check:
// on success, the returned collab.Link must be non-nil and its Identity()
// must be callable exactly the way runAgent calls it.
func TestConnectSuccessReturnsAUsableLink(t *testing.T) {
	paths := newResumeTestDaemon(t)
	factory := adaptor.NewAdaptorFactory()
	a, _ := factory.ByName("claude")
	role, err := roles.Resolve("developer")
	if err != nil {
		t.Fatal(err)
	}
	col := collab.New()

	p := Parsed{SessionKind: SessionKindLocalNew, Session: proto.SessionNew, Name: "alice"}
	lk, err := connect(paths, p, role, a, col)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if lk == nil {
		t.Fatal("expected a non-nil collab.Link on success")
	}
	defer lk.Close(0)
	if id := lk.Identity(); id.Agent.Name != "alice" || id.Session.Kind != "shared" {
		t.Fatalf("unexpected identity: %+v", id)
	}
}
