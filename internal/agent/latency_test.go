package agent

import (
	"os"
	"os/exec"
	"sort"
	"testing"
	"time"

	"github.com/creack/pty"
)

// roundTrips writes one byte, waits for its echo, n times, and returns the
// sorted latencies. The tool is `cat` on a PTY, whose line discipline echoes
// each byte immediately, so this measures pure plumbing.
func roundTrips(t testing.TB, w, r *os.File, n int) []time.Duration {
	t.Helper()
	buf := make([]byte, 64)
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		if _, err := w.Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
		out = append(out, time.Since(start))
		time.Sleep(500 * time.Microsecond) // keep round trips unbatched
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func pct(d []time.Duration, p float64) time.Duration { return d[int(float64(len(d)-1)*p)] }

func directRoundTrips(t testing.TB, n int) []time.Duration {
	cmd := exec.Command("cat")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer func() { _ = cmd.Process.Kill(); ptmx.Close() }()
	roundTrips(t, ptmx, ptmx, 50) // warm up
	return roundTrips(t, ptmx, ptmx, n)
}

func relayedRoundTrips(t testing.TB, n int) []time.Duration {
	user, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty:", err)
	}
	go Run(Config{Tool: "cat", Bin: "/bin/cat", Env: os.Environ(), In: tty, Out: tty})
	time.Sleep(300 * time.Millisecond) // let raw mode + child start
	defer func() { user.Write([]byte{0x04}); user.Close() }()
	roundTrips(t, user, user, 50)
	return roundTrips(t, user, user, n)
}

// TestLatencyOverhead reports how much Relay adds to a keystroke round trip.
// The bound is deliberately loose (this runs on shared CI/WSL); read the log
// output for the real numbers.
func TestLatencyOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("latency check skipped in -short mode")
	}
	const n = 1500
	d := directRoundTrips(t, n)
	r := relayedRoundTrips(t, n)
	t.Logf("direct: p50=%v p99=%v max=%v", pct(d, .5), pct(d, .99), d[len(d)-1])
	t.Logf("relay : p50=%v p99=%v max=%v", pct(r, .5), pct(r, .99), r[len(r)-1])
	t.Logf("added : p50=%v p99=%v", pct(r, .5)-pct(d, .5), pct(r, .99)-pct(d, .99))

	if added := pct(r, .5) - pct(d, .5); added > 2*time.Millisecond {
		t.Errorf("median added latency %v exceeds 2ms", added)
	}
	if added := pct(r, .99) - pct(d, .99); added > 10*time.Millisecond {
		t.Errorf("p99 added latency %v exceeds 10ms", added)
	}
}

func BenchmarkRoundTripDirect(b *testing.B) { directRoundTrips(b, b.N) }
func BenchmarkRoundTripRelay(b *testing.B)  { relayedRoundTrips(b, b.N) }
