package adaptor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// buildFakeAgyOnPath compiles testdata/fakeagy as a binary literally named
// "agy" (matching what ResolveBinary looks up on PATH) and prepends its
// directory to PATH for this test, so AgySweepStale's own PATH lookup finds
// it without ever touching a real agy install.
func buildFakeAgyOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name := "agy"
	if runtime.GOOS == "windows" {
		name = "agy.exe"
	}
	bin := filepath.Join(dir, name)
	out, err := exec.Command("go", "build", "-o", bin, "../../testdata/fakeagy").CombinedOutput()
	if err != nil {
		t.Skip("cannot build fakeagy (is `go` on PATH?):", fmt.Sprintf("%s: %v", out, err))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAgySweepStaleRemovesOnlyDeadEntries(t *testing.T) {
	buildFakeAgyOnPath(t)
	geminiHome := t.TempDir()
	t.Setenv("GEMINI_HOME", geminiHome)

	root := t.TempDir()
	paths := relayhome.Paths{Root: root}
	if err := os.MkdirAll(paths.RunDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	liveDir := t.TempDir()
	deadDir := filepath.Join(t.TempDir(), "gone")
	cfg := map[string]any{"mcpServers": map[string]any{
		"relay-live": map[string]any{"command": "/opt/relay/relay", "args": []string{"mcp", "--dir", liveDir}, "disabled": false},
		"relay-dead": map[string]any{"command": "/opt/relay/relay", "args": []string{"mcp", "--dir", deadDir}, "disabled": false},
	}}
	data, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(geminiHome, "config", "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := AgySweepStale(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "relay-dead" {
		t.Fatalf("removed = %v, want [relay-dead]", removed)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.MCPServers["relay-dead"]; ok {
		t.Error("relay-dead must be gone")
	}
	if _, ok := got.MCPServers["relay-live"]; !ok {
		t.Error("relay-live must survive")
	}
}

func TestAgySweepStaleIsANoOpWhenAgyIsNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on it
	removed, err := AgySweepStale(relayhome.Paths{Root: t.TempDir()})
	if err != nil || removed != nil {
		t.Fatalf("removed=%v err=%v, want a silent no-op", removed, err)
	}
}
