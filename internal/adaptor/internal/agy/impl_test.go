package agy

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
)

func TestNameAndBinary(t *testing.T) {
	a := &AgyAdaptor{}
	if a.Name() != "agy" || a.Binary() != "agy" {
		t.Fatalf("Name/Binary = %q/%q", a.Name(), a.Binary())
	}
	if a.Env() != nil {
		t.Fatalf("Env() must be nil")
	}
}

func TestPrepareNonInteractiveIsPassthrough(t *testing.T) {
	a := &AgyAdaptor{}
	for _, args := range [][]string{{"-p", "hi"}, {"--print", "hi"}, {"mcp", "list"}, {"--version"}, {"install"}, {"--help"}} {
		s := launch.Spec{RunDir: t.TempDir(), WithMCP: true, UserArgs: args}
		p, err := a.Prepare(s)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Passthrough || !reflect.DeepEqual(p.Args, args) || p.MCP {
			t.Errorf("%q must pass through untouched, got %+v", args, p)
		}
	}
}

func TestPrepareRegistersAndCleanupUnregisters(t *testing.T) {
	paths, agyBin := setup(t)
	a := &AgyAdaptor{}
	runDir := t.TempDir()
	s := launch.Spec{RunDir: runDir, RelayHome: paths.Root, RelayExe: "/opt/relay/relay", ToolBin: agyBin, WithMCP: true, UserArgs: []string{"fix bug"}}

	p, err := a.Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	if !p.MCP || p.Passthrough {
		t.Fatalf("%+v", p)
	}
	marker := filepath.Join(runDir, markerFile)
	entry, rerr := os.ReadFile(marker)
	if rerr != nil {
		t.Fatalf("marker file: %v", rerr)
	}
	names := serverNames(t)
	if !names[string(entry)] {
		t.Fatalf("registered entry %q not found: %v", entry, names)
	}

	if err := a.Cleanup(s); err != nil {
		t.Fatal(err)
	}
	names = serverNames(t)
	if names[string(entry)] {
		t.Fatalf("entry must be gone after Cleanup: %v", names)
	}
}

func TestPrepareSoloNeverTouchesAgy(t *testing.T) {
	paths, agyBin := setup(t)
	a := &AgyAdaptor{}
	runDir := t.TempDir()
	s := launch.Spec{RunDir: runDir, RelayHome: paths.Root, ToolBin: agyBin, WithMCP: false, UserArgs: []string{"a", "b"}}
	p, err := a.Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.MCP || !reflect.DeepEqual(p.Args, []string{"a", "b"}) {
		t.Fatalf("%+v", p)
	}
	if _, err := os.Stat(filepath.Join(runDir, markerFile)); !os.IsNotExist(err) {
		t.Fatal("no marker file when MCP was never requested")
	}
	if _, err := os.Stat(mustConfigPath(t)); !os.IsNotExist(err) {
		t.Fatal("agy's config must not exist: nothing was ever registered")
	}
}

func TestPrepareLeavesNoMarkerOrEntryWhenAddFails(t *testing.T) {
	paths, _ := setup(t)
	a := &AgyAdaptor{}
	runDir := t.TempDir()
	// A binary that cannot possibly be agy: Prepare must degrade to a note,
	// not fail the whole launch, and must leave nothing behind.
	s := launch.Spec{RunDir: runDir, RelayHome: paths.Root, RelayExe: "/opt/relay/relay", ToolBin: filepath.Join(t.TempDir(), "no-such-binary"), WithMCP: true, UserArgs: []string{"go"}}
	p, err := a.Prepare(s)
	if err != nil {
		t.Fatal(err) // Prepare itself must never fail; degrade via Notes instead
	}
	if p.MCP || len(p.Notes) == 0 {
		t.Fatalf("expected a degrade note and MCP=false, got %+v", p)
	}
	if _, err := os.Stat(filepath.Join(runDir, markerFile)); !os.IsNotExist(err) {
		t.Fatal("no marker file when registration failed")
	}
	// Cleanup on this same spec must be a safe no-op.
	if err := a.Cleanup(s); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupIsNoOpWithoutMarker(t *testing.T) {
	a := &AgyAdaptor{}
	s := launch.Spec{RunDir: t.TempDir()}
	if err := a.Cleanup(s); err != nil {
		t.Fatalf("Cleanup with no marker must be a safe no-op, got %v", err)
	}
}

func TestScreenRulesNonEmpty(t *testing.T) {
	a := &AgyAdaptor{}
	if len(a.ScreenRules()) == 0 {
		t.Fatal("agy has no dialog rules: relay would type into permission prompts")
	}
}

func mustConfigPath(t *testing.T) string {
	t.Helper()
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
