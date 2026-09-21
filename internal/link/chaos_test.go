package link

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/daemon"
	"github.com/thesahibnanda-max/relay/internal/proto"
)

// receiver records every delivery (at-least-once, so an id may repeat).
type receiver struct {
	mu    sync.Mutex
	ids   map[string]string // msg id -> body
	deliv int
}

func newReceiver() *receiver { return &receiver{ids: map[string]string{}} }

func (r *receiver) on(m proto.MessageView) {
	r.mu.Lock()
	r.ids[m.ID] = m.Body
	r.deliv++
	r.mu.Unlock()
}

func (r *receiver) bodies() map[string]int { // body -> number of DISTINCT message ids
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for _, b := range r.ids {
		out[b]++
	}
	return out
}

// TestChaosDaemonRestartsDuringDelivery kills and restarts the daemon over and
// over while one agent sends and another receives. Nothing may be lost, and a
// send retried across a restart must not create a second message.
func TestChaosDaemonRestartsDuringDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test")
	}
	d := newTestDaemon(t)
	d.tune = func(o *daemon.Options) { o.PairLimit, o.SenderLimit = 1<<20, 1<<20 }
	d.stop()
	d.start()

	rx := newReceiver()
	alice, err := Connect(bg, d.opts(proto.Hello{Session: proto.SessionNew, Name: "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close(0)
	o := d.opts(proto.Hello{Session: alice.Identity().Session.ID, Name: "bob"})
	o.OnDeliver = rx.on
	bob, err := Connect(bg, o)
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close(0)

	const n = 300
	stopChaos := make(chan struct{})
	var chaos sync.WaitGroup
	chaos.Add(1)
	restarts := 0
	go func() {
		defer chaos.Done()
		for {
			select {
			case <-stopChaos:
				return
			case <-time.After(time.Duration(80+rand.IntN(120)) * time.Millisecond):
				d.stop()
				time.Sleep(time.Duration(rand.IntN(60)) * time.Millisecond)
				d.start()
				restarts++
			}
		}
	}()

	for i := 0; i < n; i++ {
		var res proto.SendResult
		if err := alice.Call(bg, proto.OpSend, proto.SendArgs{To: "bob", Body: fmt.Sprintf("payload-%04d", i)}, &res); err != nil {
			t.Fatalf("send %d failed for good: %v", i, err)
		}
	}
	close(stopChaos)
	chaos.Wait()
	d.start()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && len(rx.bodies()) < n {
		time.Sleep(50 * time.Millisecond)
	}
	got := rx.bodies()
	if len(got) != n {
		t.Logf("DIAGNOSTICS\n%s\ndaemon log tail:\n%s", d.diagnose(alice.Identity().Session.ID, alice, bob), d.logs.tail(40))
		t.Fatalf("lost messages: %d of %d delivered (after %d daemon restarts)", len(got), n, restarts)
	}
	for body, ids := range got {
		if ids != 1 {
			t.Errorf("%s exists as %d different messages: a retried send was duplicated", body, ids)
		}
	}
	if restarts < 3 {
		t.Fatalf("the chaos did not happen (%d restarts)", restarts)
	}
	t.Logf("%d messages, %d daemon restarts, %d deliveries (%d redeliveries de-duplicated by id)", n, restarts, rx.deliv, rx.deliv-n)
}

// TestSoakManyAgentsTalkingAtOnce: 8 agents each fire messages at random peers
// while the daemon restarts once. Every message reaches its addressee.
func TestSoakManyAgentsTalkingAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test")
	}
	d := newTestDaemon(t)
	d.tune = func(o *daemon.Options) { o.PairLimit, o.SenderLimit = 1<<20, 1<<20 }
	d.stop()
	d.start()

	const agents, each = 8, 120
	var session string
	rx := make([]*receiver, agents)
	cl := make([]*Client, agents)
	for i := range cl {
		rx[i] = newReceiver()
		h := proto.Hello{Session: session, Name: fmt.Sprintf("a%d", i)}
		if i == 0 {
			h.Session = proto.SessionNew
		}
		o := d.opts(h)
		o.OnDeliver = rx[i].on
		c, err := Connect(bg, o)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(0)
		cl[i] = c
		if i == 0 {
			session = c.Identity().Session.ID
		}
	}

	var mu sync.Mutex
	expect := make([]map[string]bool, agents) // per recipient: bodies that must arrive
	for i := range expect {
		expect[i] = map[string]bool{}
	}
	var wg sync.WaitGroup
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < each; k++ {
				to := rand.IntN(agents)
				if to == i {
					to = (to + 1) % agents
				}
				body := fmt.Sprintf("a%d>a%d #%d", i, to, k)
				var res proto.SendResult
				if err := cl[i].Call(bg, proto.OpSend, proto.SendArgs{To: fmt.Sprintf("a%d", to), Body: body}, &res); err != nil {
					t.Errorf("send %s: %v", body, err)
					return
				}
				mu.Lock()
				expect[to][body] = true
				mu.Unlock()
				if i == 0 && k == each/2 { // one restart in the middle of the storm
					d.stop()
					d.start()
				}
			}
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		missing := 0
		for i := range rx {
			got := rx[i].bodies()
			mu.Lock()
			for b := range expect[i] {
				if got[b] == 0 {
					missing++
				}
			}
			mu.Unlock()
		}
		if missing == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	total := 0
	for i := range rx {
		got := rx[i].bodies()
		for b := range expect[i] {
			total++
			if got[b] != 1 {
				t.Errorf("agent a%d: %q delivered as %d distinct messages", i, b, got[b])
			}
		}
	}
	t.Logf("%d messages between %d agents delivered exactly once each", total, agents)
}

// diagnose summarises where messages and agents stand, for failure reports.
func (d *testDaemon) diagnose(session string, clients ...*Client) string {
	var b strings.Builder
	for _, a := range d.session(session).Agents {
		fmt.Fprintf(&b, "agent %s status=%s connected=%v\n", a.Name, a.Status, a.Connected)
	}
	c := proto.HTTPClient(d.paths.SocketPath())
	resp, err := c.Get("http://relay/v1/admin/messages?session=" + session + "&limit=1000")
	if err == nil {
		defer resp.Body.Close()
		var msgs []proto.MessageView
		json.NewDecoder(resp.Body).Decode(&msgs)
		counts := map[string]int{}
		for _, m := range msgs {
			counts[m.State]++
		}
		fmt.Fprintf(&b, "messages by state: %v (total %d)\n", counts, len(msgs))
	}
	for i, cl := range clients {
		fmt.Fprintf(&b, "client %d: online=%v err=%v dropped=%d\n", i, cl.Online(), cl.Err(), cl.Dropped())
	}
	return b.String()
}
