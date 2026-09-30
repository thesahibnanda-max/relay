package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

var (
	fakeAgyOnce sync.Once
	fakeAgyBin  string
	fakeAgyErr  error
	// buildEnv is the environment before any test points HOME at a temp
	// dir: go build under that HOME would fill it with a read-only module
	// cache (where GOPATH is unset) that t.TempDir cannot remove.
	buildEnv = os.Environ()
)

// buildFakeAgy compiles testdata/fakeagy once per test binary run: a
// stand-in for the real agy that edits its MCP config exactly as agy does
// (see its own doc comment), including agy's lack of any locking.
func buildFakeAgy(t *testing.T) string {
	t.Helper()
	fakeAgyOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeagy")
		if err != nil {
			fakeAgyErr = err
			return
		}
		name := "fakeagy"
		if runtime.GOOS == "windows" {
			name += ".exe" // go build -o with an explicit path does not add it
		}
		fakeAgyBin = filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", fakeAgyBin, "../../../../testdata/fakeagy")
		cmd.Env = buildEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			fakeAgyErr = fmt.Errorf("%s: %w", out, err)
		}
	})
	if fakeAgyErr != nil {
		if _, err := exec.LookPath("go"); err != nil {
			t.Skip("cannot build fakeagy without go on PATH")
		}
		t.Fatal("cannot build fakeagy:", fakeAgyErr) // a broken build must never pass as a skip
	}
	return fakeAgyBin
}

// setHome points the user's home directory (which both this package and
// fakeagy resolve agy's files from, exactly as the real agy does) at a
// fresh temp dir, so no test ever touches a real ~/.gemini.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

type rig struct {
	t    *testing.T
	home string
	opt  Options
	cfg  string
	dead map[int]bool
	mu   sync.Mutex
}

// newRig returns Options wired to fakeagy and a fake liveness table: a lease
// written by this test process counts as alive unless marked dead.
func newRig(t *testing.T) *rig {
	t.Helper()
	home := setHome(t)
	r := &rig{t: t, home: home, dead: map[int]bool{}}
	r.opt = Options{AgyBin: buildFakeAgy(t), RelayExe: "/opt/relay/bin/relay", Alive: r.alive}
	r.cfg = filepath.Join(home, ".gemini", "config", "mcp_config.json")
	return r
}

func (r *rig) alive(pid int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return pid > 0 && !r.dead[pid]
}

// crash makes every lease written so far (all carry this process's pid)
// look like it belongs to a process killed with kill -9.
func (r *rig) crash() {
	r.mu.Lock()
	r.dead[os.Getpid()] = true
	r.mu.Unlock()
}

func (r *rig) revive() {
	r.mu.Lock()
	delete(r.dead, os.Getpid())
	r.mu.Unlock()
}

func (r *rig) write(data string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.cfg), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.cfg, []byte(data), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) read() (string, bool) {
	data, err := os.ReadFile(r.cfg)
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return string(data), true
}

func (r *rig) servers() map[string]json.RawMessage {
	r.t.Helper()
	data, ok := r.read()
	if !ok || len(bytes.TrimSpace([]byte(data))) == 0 {
		return nil
	}
	var top struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(data), &top); err != nil {
		r.t.Fatalf("invalid JSON in agy config: %v (%s)", err, data)
	}
	return top.MCPServers
}

func (r *rig) register(agentID string) {
	r.t.Helper()
	if _, err := Register(r.opt, agentID); err != nil {
		r.t.Fatalf("Register(%s): %v", agentID, err)
	}
}

func (r *rig) unregister(agentID string) {
	r.t.Helper()
	if err := Unregister(r.opt, agentID); err != nil {
		r.t.Fatalf("Unregister(%s): %v", agentID, err)
	}
}

// assertNoTrace checks nothing of relay's is left in agy's directories.
func (r *rig) assertNoTrace() {
	r.t.Helper()
	for _, p := range []string{
		filepath.Join(r.home, ".gemini", "config", stateDirName),
		filepath.Join(r.home, ".gemini", "antigravity-cli", "mcp", ServerName),
	} {
		if _, err := os.Stat(p); err == nil {
			r.t.Errorf("%s left behind", p)
		}
	}
}

func (r *rig) assertOurEntry() {
	r.t.Helper()
	raw, ok := r.servers()[ServerName]
	if !ok {
		r.t.Fatalf("no %q entry; config: %v", ServerName, r.servers())
	}
	var e struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	json.Unmarshal(raw, &e)
	if e.Command != r.opt.RelayExe || len(e.Args) != 2 || e.Args[0] != "mcp" || e.Args[1] != "--from-env" {
		r.t.Fatalf("entry = %s, want %s mcp --from-env", raw, r.opt.RelayExe)
	}
}

const agentA, agentB = "01K6AAAAAAAAAAAAAAAAAAAAAA", "01K6BBBBBBBBBBBBBBBBBBBBBB"

func TestRegisterThenUnregisterRestoresNonexistence(t *testing.T) {
	r := newRig(t)
	r.register(agentA)
	r.assertOurEntry()
	// agy caches tool schemas per server name; relay's goes when relay does.
	cache := filepath.Join(r.home, ".gemini", "antigravity-cli", "mcp", ServerName)
	os.MkdirAll(cache, 0o755)
	os.WriteFile(filepath.Join(cache, "relay_send.json"), []byte("{}"), 0o644)
	r.unregister(agentA)
	if data, ok := r.read(); ok {
		t.Fatalf("agy config should not exist again, has %q", data)
	}
	r.assertNoTrace()
}

// Confirmed live: a 0-byte mcp_config.json is what a fresh agy install has,
// and it broke registration outright before (issue #50's missing tools).
func TestEmptyFileRegistersAndComesBackEmpty(t *testing.T) {
	for _, orig := range []string{"", "  \n"} {
		r := newRig(t)
		r.write(orig)
		r.register(agentA)
		r.assertOurEntry()
		r.unregister(agentA)
		if data, ok := r.read(); !ok || data != orig {
			t.Fatalf("config = %q (exists %v), want the original %q back byte for byte", data, ok, orig)
		}
		r.assertNoTrace()
	}
}

// agy rewrites the whole file (sorted keys, its own indentation) on every
// add/remove; relay puts the user's exact bytes back.
func TestUserContentRestoredByteForByte(t *testing.T) {
	r := newRig(t)
	orig := "{\"mcpServers\":{\"mine\":{\"command\":\"echo\",\"args\":[\"a\"],\"env\":{\"K\":\"v\"}}},\n   \"other\": 5}"
	r.write(orig)
	r.register(agentA)
	if _, ok := r.servers()["mine"]; !ok {
		t.Fatal("the user's own server was lost")
	}
	r.unregister(agentA)
	if data, _ := r.read(); data != orig {
		t.Fatalf("config = %q, want the original bytes %q", data, orig)
	}
}

func TestUserChangesDuringSessionAreKept(t *testing.T) {
	r := newRig(t)
	r.write(`{"mcpServers":{}}`)
	r.register(agentA)
	if out, err := exec.Command(r.opt.AgyBin, "mcp", "add", "added-meanwhile", "--", "echo", "x").CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	r.unregister(agentA)
	s := r.servers()
	if _, ok := s["added-meanwhile"]; !ok {
		t.Fatalf("a server the user added during the session was lost: %v", s)
	}
	if _, ok := s[ServerName]; ok {
		t.Fatal("relay's entry left behind")
	}
}

func TestEntryStaysUntilTheLastAgentLeaves(t *testing.T) {
	r := newRig(t)
	r.register(agentA)
	r.register(agentB)
	r.unregister(agentA)
	r.assertOurEntry()
	r.unregister(agentB)
	if _, ok := r.read(); ok {
		t.Fatal("entry not removed after the last agent left")
	}
	r.assertNoTrace()
}

// kill -9 of relay leaves its lease and entry behind; the next launch must
// clean up the dead one's share and still restore the original at the end.
func TestCrashedLaunchIsCleanedUpByTheNext(t *testing.T) {
	r := newRig(t)
	orig := `{"mcpServers":{"mine":{"command":"echo"}}}`
	r.write(orig)
	r.register(agentA)
	// agentA's relay is killed without unregistering: its lease now names a dead pid.
	const deadPID = 999999999
	r.mu.Lock()
	r.dead[deadPID] = true
	r.mu.Unlock()
	stale, _ := json.Marshal(lease{AgentID: agentA, PID: deadPID, Started: time.Now()})
	os.WriteFile(filepath.Join(r.home, ".gemini", "config", stateDirName, "leases", agentA+".json"), stale, 0o600)
	r.register(agentB)
	r.assertOurEntry()
	r.unregister(agentB)
	if data, _ := r.read(); data != orig {
		t.Fatalf("config = %q, want the original %q", data, orig)
	}
	r.assertNoTrace()
}

func TestSweepCleansUpAfterACrashButNotAfterALiveAgent(t *testing.T) {
	r := newRig(t)
	r.register(agentA)
	if removed, err := Sweep(r.opt); err != nil || len(removed) != 0 {
		t.Fatalf("Sweep with a live agent = %v, %v; want nothing removed", removed, err)
	}
	r.assertOurEntry()
	r.crash()
	removed, err := Sweep(r.opt)
	if err != nil || len(removed) != 1 || removed[0] != ServerName {
		t.Fatalf("Sweep after a crash = %v, %v; want [%s]", removed, err, ServerName)
	}
	if _, ok := r.read(); ok {
		t.Fatal("config should be gone again")
	}
	r.assertNoTrace()
}

func TestSweepWithNothingRegisteredTouchesNothing(t *testing.T) {
	r := newRig(t)
	r.write(`{"mcpServers":{"mine":{"command":"echo"}}}`)
	if removed, err := Sweep(r.opt); err != nil || len(removed) != 0 {
		t.Fatalf("Sweep = %v, %v", removed, err)
	}
	if data, _ := r.read(); data != `{"mcpServers":{"mine":{"command":"echo"}}}` {
		t.Fatalf("config changed: %q", data)
	}
	r.assertNoTrace()
}

func TestForeignRelayEntryIsNeverTouched(t *testing.T) {
	r := newRig(t)
	orig := `{"mcpServers":{"relay":{"command":"/usr/local/bin/some-other-relay","args":["serve"]}}}`
	r.write(orig)
	if _, err := Register(r.opt, agentA); err == nil {
		t.Fatal("Register over a foreign \"relay\" entry must fail")
	}
	if data, _ := r.read(); data != orig {
		t.Fatalf("config changed: %q", data)
	}
	r.assertNoTrace()
}

// agy refuses non-JSON configs too (comments, a BOM, trailing commas -
// confirmed live), so relay leaves them alone and says so.
func TestUnparseableConfigIsLeftAlone(t *testing.T) {
	for _, orig := range []string{"{ // comment\n}", "\xef\xbb\xbf{}", `{"mcpServers": {"x": }`} {
		r := newRig(t)
		r.write(orig)
		_, err := Register(r.opt, agentA)
		if !errors.Is(err, ErrUnparseable) {
			t.Fatalf("Register(%q) err = %v, want ErrUnparseable", orig, err)
		}
		if data, _ := r.read(); data != orig {
			t.Fatalf("config changed: %q", data)
		}
		r.assertNoTrace()
	}
}

// agy accepts and keeps odd user entries (confirmed live: "args" as a
// string); relay must not fail on them.
func TestOddUserEntriesAreTolerated(t *testing.T) {
	r := newRig(t)
	orig := `{"mcpServers":{"odd":{"command":5,"args":"notalist"},"web":{"serverUrl":"https://x"}}}`
	r.write(orig)
	r.register(agentA)
	r.assertOurEntry()
	r.unregister(agentA)
	if data, _ := r.read(); data != orig {
		t.Fatalf("config = %q, want %q", data, orig)
	}
}

func TestLegacyPerLaunchEntriesAreSwept(t *testing.T) {
	r := newRig(t)
	liveDir := t.TempDir()
	info, _ := json.Marshal(relayhome.RunInfo{AgentID: agentB, PID: os.Getpid()})
	os.WriteFile(filepath.Join(liveDir, "agent.json"), info, 0o600)
	r.write(fmt.Sprintf(`{"mcpServers":{
		"relay-01K6CCCCCCCCCCCCCCCCCCCCCC":{"command":"/r","args":["mcp","--dir","/nonexistent/a"]},
		"relay-01K6DDDDDDDDDDDDDDDDDDDDDD":{"command":"/r","args":["mcp","--dir",%q]},
		"relay-notours":{"command":"/r","args":["other"]}}}`, liveDir))
	notes, err := Register(r.opt, agentA)
	if err != nil {
		t.Fatal(err)
	}
	s := r.servers()
	if _, ok := s["relay-01K6CCCCCCCCCCCCCCCCCCCCCC"]; ok {
		t.Error("a legacy entry for a gone run dir was not swept")
	}
	if _, ok := s["relay-01K6DDDDDDDDDDDDDDDDDDDDDD"]; !ok {
		t.Error("a legacy entry still owned by a live (older) relay was swept")
	}
	if _, ok := s["relay-notours"]; !ok {
		t.Error("an entry relay never registered was swept")
	}
	if len(notes) == 0 {
		t.Error("no note about the swept entry")
	}
}

// An older relay recorded, per RELAY_HOME, that the file did not exist
// before it; that answer is honoured once so the empty shell it left goes.
func TestLegacyExistenceMarkerIsHonoured(t *testing.T) {
	r := newRig(t)
	r.write("{\n  \"mcpServers\": {}\n}\n")
	marker := filepath.Join(t.TempDir(), "agy-mcp-original.json")
	os.WriteFile(marker, []byte(`{"existed":false}`), 0o600)
	r.opt.LegacyMarker = marker
	r.register(agentA)
	if _, err := os.Stat(marker); err == nil {
		t.Error("legacy marker not removed once honoured")
	}
	r.unregister(agentA)
	if _, ok := r.read(); ok {
		t.Fatal("the empty shell an older relay created should be gone")
	}
}

func TestInspect(t *testing.T) {
	r := newRig(t)
	st, err := Inspect(r.opt)
	if err != nil || st.Registered || st.Stale() {
		t.Fatalf("fresh: %+v %v", st, err)
	}
	r.register(agentA)
	st, _ = Inspect(r.opt)
	if !st.Registered || st.Stale() || len(st.LiveAgents) != 1 {
		t.Fatalf("live: %+v", st)
	}
	r.crash()
	st, _ = Inspect(r.opt)
	if !st.Stale() {
		t.Fatalf("after crash: %+v, want stale", st)
	}
	r.write("{nope")
	st, _ = Inspect(r.opt)
	if st.ParseError == nil {
		t.Fatal("unparseable config not reported")
	}
}

func TestLockTimesOut(t *testing.T) {
	r := newRig(t)
	old := lockWait
	lockWait = 200 * time.Millisecond
	defer func() { lockWait = old }()
	s, _ := r.opt.state()
	os.MkdirAll(s.dir, 0o700)
	held, err := agyFlock(filepath.Join(s.dir, "lock"), func() error { return nil }, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := Register(r.opt, agentA); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("Register while locked: %v, want ErrLockTimeout", err)
	}
}

// Many relays launching agy at once must each end up with a lease, the one
// entry registered, and - after all leave - the file exactly as it was.
func TestConcurrentLaunchesThroughTheLockLoseNothing(t *testing.T) {
	r := newRig(t)
	t.Setenv("FAKEAGY_DELAY_MS", "20")
	orig := `{"mcpServers":{"mine":{"command":"echo"}}}`
	r.write(orig)
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("01K6EEEEEEEEEEEEEEEEEEEEE%d", i)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := Register(r.opt, id); err != nil {
				errs <- err
			}
		}(ids[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	st, _ := Inspect(r.opt)
	if len(st.LiveAgents) != n {
		t.Fatalf("%d leases, want %d", len(st.LiveAgents), n)
	}
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := Unregister(r.opt, id); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	if data, _ := r.read(); data != orig {
		t.Fatalf("config = %q, want %q", data, orig)
	}
	r.assertNoTrace()
}

// The lock above is what prevents the loss, not fakeagy being unable to
// lose writes: called directly, concurrently, fakeagy (like the real agy)
// loses some. If this ever stops reproducing, the test above proves nothing.
func TestFakeAgyLosesWritesWithoutTheLock(t *testing.T) {
	r := newRig(t)
	t.Setenv("FAKEAGY_DELAY_MS", "30")
	const n = 10
	lost := false
	for attempt := 0; attempt < 5 && !lost; attempt++ {
		os.Remove(r.cfg)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				exec.Command(r.opt.AgyBin, "mcp", "add", fmt.Sprintf("race-%d", i), "--", "/bin/true").Run()
			}(i)
		}
		wg.Wait()
		lost = len(r.servers()) < n
	}
	if !lost {
		t.Fatal("fakeagy never lost a concurrent write: it no longer models agy's missing locking")
	}
}
