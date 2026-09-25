package adaptor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFactoryByName(t *testing.T) {
	f := NewAdaptorFactory()
	for _, name := range []string{"claude", "CLAUDE", "  codex ", "copilot", "COPILOT"} {
		if _, ok := f.ByName(name); !ok {
			t.Errorf("ByName(%q) not found", name)
		}
	}
	for _, name := range []string{"", "gemini", "relay"} {
		if _, ok := f.ByName(name); ok {
			t.Errorf("ByName(%q) unexpectedly found", name)
		}
	}
}

func writeExe(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSkipsRelayShim(t *testing.T) {
	shimDir, realDir := t.TempDir(), t.TempDir()

	relay := filepath.Join(shimDir, "relay")
	writeExe(t, relay)
	if err := os.Symlink(relay, filepath.Join(shimDir, "claude")); err != nil {
		t.Skip("symlinks unsupported here:", err)
	}
	real := filepath.Join(realDir, "claude")
	writeExe(t, real)

	// shim dir comes first on PATH, as it would when a user shadows `claude`.
	path := shimDir + string(os.PathListSeparator) + realDir
	got, err := resolveIn("claude", path, relay)
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Errorf("resolved %q, want %q", got, real)
	}
}

func TestResolveNotFound(t *testing.T) {
	_, err := resolveIn("claude", t.TempDir(), "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestChildEnvMarksRelay(t *testing.T) {
	t.Setenv(EnvActive, "")
	os.Unsetenv(EnvActive)
	f := NewAdaptorFactory()
	a, _ := f.ByName("claude")
	if !hasKey(ChildEnv(a), EnvActive) {
		t.Errorf("%s missing from child env", EnvActive)
	}
}
