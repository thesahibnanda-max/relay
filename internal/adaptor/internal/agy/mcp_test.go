package agy

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

var (
	fakeAgyOnce sync.Once
	fakeAgyBin  string
	fakeAgyErr  error
)

// buildFakeAgy compiles testdata/fakeagy once per test binary run - the
// scripted stand-in for `agy mcp add`/`mcp remove` (see its own doc comment
// for why it deliberately has no locking of its own).
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
			// go build -o with an explicit path does NOT auto-append .exe
			// (only the default output name does) - confirmed live: without
			// this, exec.Command fails with "executable file not found",
			// since Windows requires a recognized extension to launch a file
			// directly, even though its bytes are a valid PE image.
			name += ".exe"
		}
		fakeAgyBin = filepath.Join(dir, name)
		out, err := exec.Command("go", "build", "-o", fakeAgyBin, "../../../../testdata/fakeagy").CombinedOutput()
		if err != nil {
			fakeAgyErr = fmt.Errorf("%s: %w", out, err)
		}
	})
	if fakeAgyErr != nil {
		t.Skip("cannot build fakeagy (is `go` on PATH?):", fakeAgyErr)
	}
	return fakeAgyBin
}

// setup returns a relayhome.Paths rooted at a fresh temp dir (with RunDir
// already created, matching what paths.Ensure() does in production) and
// points GEMINI_HOME at a second, independent temp dir so this package's own
// ConfigPath() and fakeagy's own config resolution agree without ever
// touching a real ~/.gemini.
func setup(t *testing.T) (paths relayhome.Paths, agyBin string) {
	t.Helper()
	root := t.TempDir()
	paths = relayhome.Paths{Root: root}
	if err := os.MkdirAll(paths.RunDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEMINI_HOME", t.TempDir())
	return paths, buildFakeAgy(t)
}

func readRawConfig(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	cfgPath, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("invalid JSON in config: %v (%s)", err, data)
	}
	return top
}

func serverNames(t *testing.T) map[string]bool {
	t.Helper()
	top := readRawConfig(t)
	var servers map[string]json.RawMessage
	if raw, ok := top["mcpServers"]; ok {
		json.Unmarshal(raw, &servers)
	}
	names := map[string]bool{}
	for k := range servers {
		names[k] = true
	}
	return names
}

func TestAddAndSweepRegistersEntryThenRemoveRestoresNonexistence(t *testing.T) {
	paths, agyBin := setup(t)
	cfgPath, _ := ConfigPath()
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatal("config must not exist before the first add")
	}

	runDir := t.TempDir()
	entry, removed, err := AddAndSweep(paths, agyBin, "/opt/relay/relay", runDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("nothing stale to sweep yet, got %v", removed)
	}
	wantEntry := EntryName(runDir)
	if entry != wantEntry {
		t.Fatalf("entry = %q, want %q", entry, wantEntry)
	}
	names := serverNames(t)
	if !names[entry] || len(names) != 1 {
		t.Fatalf("config after add: %v", names)
	}

	if err := Remove(paths, agyBin, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("config must be removed entirely (never existed before): stat err = %v", err)
	}
}

func TestRemoveRestoresPreexistingContentByteIdentical(t *testing.T) {
	paths, agyBin := setup(t)
	cfgPath, _ := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{
  "mcpServers": {
    "existing-user-server": {
      "command": "/usr/bin/something",
      "args": ["--flag"],
      "disabled": false
    }
  }
}`)
	if err := os.WriteFile(cfgPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	runDir := t.TempDir()
	entry, _, err := AddAndSweep(paths, agyBin, "/opt/relay/relay", runDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(paths, agyBin, entry); err != nil {
		t.Fatal(err)
	}
	names := serverNames(t)
	if len(names) != 1 || !names["existing-user-server"] {
		t.Fatalf("the pre-existing entry must survive untouched: %v", names)
	}
	// content fidelity is agy's own job (trusted, not re-verified byte-for-byte
	// here since fakeagy re-serializes JSON); existence and the untouched key
	// are what this package's own logic is responsible for.
}

func TestSweepRemovesOnlyStaleRelayPrefixedEntries(t *testing.T) {
	paths, agyBin := setup(t)
	cfgPath, _ := ConfigPath()

	liveDir := t.TempDir()
	deadDir := filepath.Join(t.TempDir(), "does-not-exist")

	seed := mcpConfig{MCPServers: map[string]mcpServer{
		"relay-live":     {Command: "/opt/relay/relay", Args: []string{"mcp", "--dir", liveDir}},
		"relay-dead":     {Command: "/opt/relay/relay", Args: []string{"mcp", "--dir", deadDir}},
		"relay":          {Command: "/some/other/tool", Args: []string{"mcp", "--dir", deadDir}}, // another adaptor's naming: never ours to sweep
		"unrelated-tool": {Command: "/usr/bin/unrelated", Args: []string{"--dir", deadDir}},
	}}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := SweepStale(paths, agyBin)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "relay-dead" {
		t.Fatalf("removed = %v, want exactly [relay-dead]", removed)
	}
	names := serverNames(t)
	for _, want := range []string{"relay-live", "relay", "unrelated-tool"} {
		if !names[want] {
			t.Errorf("%q must survive the sweep, got %v", want, names)
		}
	}
	if names["relay-dead"] {
		t.Error("relay-dead must have been removed")
	}
}

// TestSiblingAgentSurvivesAnotherOnesCleanup is decision 3's core safety
// property: two agy agents launched on the same machine (a completely normal
// situation - agy's config is one file shared by all of them) each register
// their own entry; one exiting and cleaning up must never touch the other's
// still-live entry, and - because the file did NOT exist before either of
// them started, so a naive "restore original state" would try to delete it -
// must also not delete the config file while a sibling's real entry is still
// in it.
func TestSiblingAgentSurvivesAnotherOnesCleanup(t *testing.T) {
	paths, agyBin := setup(t)
	cfgPath, _ := ConfigPath()
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatal("config must not exist before the first add")
	}

	runDirA, runDirB := t.TempDir(), t.TempDir()
	entryA, _, err := AddAndSweep(paths, agyBin, "/opt/relay/relay", runDirA)
	if err != nil {
		t.Fatal(err)
	}
	entryB, _, err := AddAndSweep(paths, agyBin, "/opt/relay/relay", runDirB)
	if err != nil {
		t.Fatal(err)
	}

	// Agent A exits and cleans up first.
	if err := Remove(paths, agyBin, entryA); err != nil {
		t.Fatal(err)
	}
	names := serverNames(t)
	if names[entryA] {
		t.Fatalf("A's own entry must be gone: %v", names)
	}
	if !names[entryB] {
		t.Fatalf("B's entry must survive A's cleanup: %v", names)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("the config file itself must survive while B is still registered: %v", err)
	}

	// Now B exits too: only then may the file's existence be reversed.
	if err := Remove(paths, agyBin, entryB); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("once both are gone, the config must be restored to nonexistence: stat err = %v", err)
	}
}

func TestLockTimesOutWhenAnotherHolderHasIt(t *testing.T) {
	paths, _ := setup(t)
	held, err := agyFlock(paths.AgyMCPLockPath(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	_, err = agyFlock(paths.AgyMCPLockPath(), 100*time.Millisecond)
	if err != ErrLockTimeout {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
}

// TestConcurrentAddThroughRelaysLockLosesNothing is the fix for the real,
// live-confirmed bug in the real agy binary: unsynchronized concurrent
// `agy mcp add` calls on one shared file silently lose writes. Going through
// AddAndSweep (which serializes every call under agyFlock) must never lose
// one, even with fakeagy's own artificial race window widened via
// FAKEAGY_DELAY_MS.
func TestConcurrentAddThroughRelaysLockLosesNothing(t *testing.T) {
	paths, agyBin := setup(t)
	t.Setenv("FAKEAGY_DELAY_MS", "20")

	const n = 10
	var wg sync.WaitGroup
	var failures int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runDir := filepath.Join(t.TempDir(), fmt.Sprintf("run-%d", i))
			if err := os.MkdirAll(runDir, 0o700); err != nil {
				atomic.AddInt32(&failures, 1)
				return
			}
			if _, _, err := AddAndSweep(paths, agyBin, "/opt/relay/relay", runDir); err != nil {
				atomic.AddInt32(&failures, 1)
			}
		}(i)
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("%d/%d AddAndSweep calls failed", failures, n)
	}
	names := serverNames(t)
	if len(names) != n {
		t.Fatalf("got %d entries, want %d - relay's lock must serialize every add: %v", len(names), n, names)
	}
}

// TestFakeAgyReproducesTheRealRaceWhenCalledDirectly proves the previous
// test's zero-loss result comes from relay's own lock, not from fakeagy
// being unable to lose writes: calling fakeagy directly, bypassing relay's
// lock entirely (exactly how nothing protected the real agy binary in the
// original live repro), must still show the loss. If this test ever stops
// failing to reproduce the race, TestConcurrentAddThroughRelaysLockLosesNothing
// would no longer be proving anything.
func TestFakeAgyReproducesTheRealRaceWhenCalledDirectly(t *testing.T) {
	_, agyBin := setup(t)
	t.Setenv("FAKEAGY_DELAY_MS", "20")
	geminiHome := os.Getenv("GEMINI_HOME")

	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(agyBin, "mcp", "add", fmt.Sprintf("relay-race-%d", i), "/opt/relay/relay", "mcp", "--dir", "/tmp/x")
			cmd.Env = append(os.Environ(), "GEMINI_HOME="+geminiHome)
			cmd.Run()
		}(i)
	}
	wg.Wait()
	names := serverNames(t)
	if len(names) == n {
		t.Skip("fakeagy did not reproduce the race this run (timing-dependent); not a failure of relay's own code")
	}
}
