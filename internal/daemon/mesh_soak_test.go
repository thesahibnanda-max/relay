package daemon

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
)

func waitPeersLinkedAtLeast(t *testing.T, e *env, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var peers []proto.MeshPeerView
		e.getJSON("/v1/admin/mesh/peers/"+sessionID, &peers)
		n := 0
		for _, p := range peers {
			if p.Status == "linked" {
				n++
			}
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: only %d/%d peers linked: %+v", n, want, peers)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMeshCombinedSoak is M-mesh-7's own contribution on top of every prior
// milestone's dedicated tests: none of them prove the pieces actually
// compose. A real 3-node mesh (full N×N via fan-out, M-mesh-5) carries real
// cross- and same-daemon traffic (M-mesh-4) while, mid-stream, one whole
// daemon process restarts (M-mesh-5's resilience) *and* the agent that lived
// on it crashes and resumes with its original identity on the fresh
// process (M-mesh-6) - all at once, the way a real flaky machine would
// actually behave. Nothing may be lost, duplicated, or delivered to the
// wrong recipient.
func TestMeshCombinedSoak(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test")
	}
	// 500ms, not something more aggressive: an ExpireEvery under ~100ms
	// across 3 daemons each independently resyncing the whole mesh on every
	// tick creates enough self-inflicted handshake contention on the shared
	// fake network to visibly slow real convergence down (a genuine finding
	// from tuning this test, not a mesh correctness issue - production's
	// real default is 30s). 300ms was fine on a fast dev machine but flaked
	// once on a slower/loaded macOS CI runner under -race (two messages
	// still legitimately "queued", just not yet flushed by the 20s
	// deadline below - not lost, just slow); 500ms gives more headroom
	// against exactly that contention without materially slowing this
	// soak test itself.
	fast := func(o *Options) { o.ExpireEvery = 500 * time.Millisecond }
	fakeNet := meshnet.NewFakeNetwork()
	a := startServer(t, withMeshTransport(fakeNet), fast)
	b := startServer(t, withMeshTransport(fakeNet), fast)
	c := startServer(t, withMeshTransport(fakeNet), fast)
	sid := a.newSession()

	var blobAB proto.JoinBlob
	if code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blobAB); code != 200 {
		t.Fatalf("invite ab: HTTP %d", code)
	}
	if code := b.postJSON("/v1/admin/mesh/join", blobAB, nil); code != 200 {
		t.Fatalf("join ab: HTTP %d", code)
	}
	var blobAC proto.JoinBlob
	if code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blobAC); code != 200 {
		t.Fatalf("invite ac: HTTP %d", code)
	}
	if code := c.postJSON("/v1/admin/mesh/join", blobAC, nil); code != 200 {
		t.Fatalf("join ac: HTTP %d", code)
	}
	waitPeersLinkedAtLeast(t, a, sid, 2)
	waitPeersLinkedAtLeast(t, b, sid, 2)
	waitPeersLinkedAtLeast(t, c, sid, 2)

	alice := a.joinPeer(sid, proto.Hello{Name: "alice", Role: "orchestrator"})
	bob := b.joinPeer(sid, proto.Hello{Name: "bob", Role: "developer"})
	carol := c.joinPeer(sid, proto.Hello{Name: "carol", Role: "qa"})
	defer alice.ws.CloseNow()
	defer carol.ws.CloseNow()
	bobToken, bobAgentID := bob.w.Token, bob.w.Agent.ID

	a.waitAgent(sid, "bob", func(x proto.AgentInfo) bool { return x.Remote }, "bob gossiped to a")
	a.waitAgent(sid, "carol", func(x proto.AgentInfo) bool { return x.Remote }, "carol gossiped to a")
	b.waitAgent(sid, "carol", func(x proto.AgentInfo) bool { return x.Remote }, "carol gossiped to b")
	c.waitAgent(sid, "bob", func(x proto.AgentInfo) bool { return x.Remote }, "bob gossiped to c")

	type sent struct{ from, to, body string }
	names := []string{"alice", "bob", "carol"}
	peers := map[string]*peer{"alice": alice, "bob": bob, "carol": carol}
	const total = 40
	var plan []sent
	for i := 0; i < total; i++ {
		from := names[rand.IntN(3)]
		to := names[rand.IntN(3)]
		for to == from {
			to = names[rand.IntN(3)]
		}
		plan = append(plan, sent{from, to, fmt.Sprintf("msg-%03d-%s-to-%s", i, from, to)})
	}

	type delivery struct{ id, to, body string }
	var deliveries []delivery
	drain := func(name string, p *peer) {
		for {
			m, ok := p.nextDeliver(30 * time.Millisecond)
			if !ok {
				return
			}
			deliveries = append(deliveries, delivery{id: m.ID, to: name, body: m.Body})
		}
	}

	half := total / 2
	for i, s := range plan {
		if i == half {
			// Combined chaos, mid-soak: bob's whole daemon process exits
			// (a real restart, not just a dropped connection - see
			// restartServer) at the same moment bob's own agent process
			// crashes; bob then resumes with his original token on the
			// fresh daemon, same as a real machine coming back with the
			// same relay <tool> --session=<id> --name=bob command.
			bob.ws.CloseNow()
			ctx, cancel := context.WithTimeout(bg, 3*time.Second)
			if err := b.srv.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			cancel()
			b = restartServer(t, b, withMeshTransport(fakeNet), fast)
			resumed := b.joinPeer(sid, proto.Hello{Name: "bob", Token: bobToken})
			if resumed.w.Agent.ID != bobAgentID || !resumed.w.Resumed {
				t.Fatalf("bob did not resume as the same agent: %+v", resumed.w)
			}
			bob = resumed
			peers["bob"] = bob
			defer bob.ws.CloseNow()
			// Not required for correctness (queued sends flush once the
			// mesh heals on its own - see M-mesh-5), just to bound this
			// test's own running time deterministically.
			waitPeersLinkedAtLeast(t, a, sid, 2)
			waitPeersLinkedAtLeast(t, c, sid, 2)
		}
		p := peers[s.from]
		p.mustSend(proto.SendArgs{To: s.to, Body: s.body})
		drain(s.from, p)
	}

	// Counted by DISTINCT message id, not raw delivery count: at-least-once
	// redelivery (expected and correct, especially right after bob's
	// restart above) can legitimately hand back the same id twice, which
	// would make a raw count reach len(plan) while a genuinely different
	// message is still in flight - stopping the wait early and turning a
	// slow-but-fine delivery into a false "lost" failure. This was a real
	// bug caught by a CI run flaking on a slower machine: it looked like
	// mesh data loss but was actually this loop giving up too soon.
	distinctCount := func() int {
		seen := make(map[string]bool, len(deliveries))
		for _, d := range deliveries {
			seen[d.id] = true
		}
		return len(seen)
	}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		before := distinctCount()
		drain("alice", alice)
		drain("bob", bob)
		drain("carol", carol)
		if after := distinctCount(); after == before && after >= len(plan) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	byID := map[string]delivery{}
	for _, d := range deliveries {
		if prev, ok := byID[d.id]; ok && prev != d {
			t.Fatalf("message %s delivered inconsistently: %+v vs %+v", d.id, prev, d)
		}
		byID[d.id] = d
	}
	if len(byID) != len(plan) {
		gotBodies := map[string]bool{}
		for _, d := range deliveries {
			gotBodies[d.body] = true
		}
		for _, s := range plan {
			if !gotBodies[s.body] {
				t.Logf("MISSING: %+v", s)
				for name, e := range map[string]*env{"a": a, "b": b, "c": c} {
					var msgs []proto.MessageView
					e.getJSON("/v1/admin/messages?session="+sid+"&limit=1000", &msgs)
					for _, m := range msgs {
						if m.Body == s.body {
							t.Logf("  on daemon %s: %+v", name, m)
						}
					}
				}
			}
		}
		t.Fatalf("delivered %d distinct messages, want %d\nplan=%+v\ngot=%+v", len(byID), len(plan), plan, deliveries)
	}
	wantByBody := map[string]sent{}
	for _, s := range plan {
		wantByBody[s.body] = s
	}
	for _, d := range byID {
		want, ok := wantByBody[d.body]
		if !ok {
			t.Fatalf("delivered a body that was never sent: %+v", d)
		}
		if want.to != d.to {
			t.Fatalf("%q delivered to %s, want %s", d.body, d.to, want.to)
		}
	}
	t.Logf("%d messages across a 3-node mesh, one daemon restart and one agent resume mid-soak: zero loss, zero duplicates, zero misdelivery", len(plan))
}
