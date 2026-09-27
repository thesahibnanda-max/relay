// Package agy wraps Google's Antigravity CLI (the real invocable command is
// `agy`, not the marketing name "antigravity"). Unlike Claude/Codex/Copilot,
// agy has no per-invocation MCP-server-registration flag: the only way to
// register one is `agy mcp add`, which writes into agy's own persistent,
// global ~/.gemini/config/mcp_config.json. This file holds the add/remove/
// sweep/restore logic that makes registering there anyway safe under
// relay's real, concurrent, crash-prone usage pattern - see the design
// decisions in the accepted plan for issue #16 for the reasoning; in short:
//
//   - Every add/remove/sweep runs under agyFlock (lock_unix.go/lock_windows.go),
//     because agy itself does no locking at all around this file - confirmed
//     live: 10 concurrent `agy mcp add` calls silently lost one addition.
//   - Content fidelity on remove is trusted entirely to `agy mcp remove`
//     itself (confirmed live to restore pre-existing content byte-identical);
//     relay never re-implements or second-guesses that by writing its own
//     copy of the file back.
//   - The only gap `agy mcp remove` leaves is existence: on a machine where
//     the file never existed before, remove leaves a trivial empty shell
//     instead of true nonexistence. restoreLocked closes that gap, but only
//     when it can prove doing so is safe (see its own doc comment) - a naive
//     snapshot-and-restore would risk destroying a sibling agent's live
//     registration and is deliberately not what this does.
//   - A per-launch staleness sweep (sweepLocked) removes any leftover
//     relay-* entry whose owning run directory is gone, covering the one
//     case signal-based cleanup cannot reach: `kill -9` of relay itself.
package agy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

const (
	lockWait    = 5 * time.Second
	cmdTimeout  = 15 * time.Second
	entryPrefix = "relay-"
)

// EntryName is the MCP server name relay registers for one launch: never the
// bare "relay" (unlike Claude/Copilot's single, per-launch, ephemeral config
// file), because agy's mcp_config.json is one file shared by every
// concurrently running agy agent - a fixed name would collide.
func EntryName(runDir string) string {
	return entryPrefix + filepath.Base(runDir)
}

// geminiHome resolves the directory agy itself uses. GEMINI_HOME is not
// confirmed to be honored by the real agy binary (unlike CODEX_HOME/
// COPILOT_HOME, which the real Codex/Copilot binaries do honor); it exists
// here only so tests can point a fake agy binary and this package at the
// same temporary HOME without needing the real environment variable.
func geminiHome() (string, error) {
	if h := os.Getenv("GEMINI_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini"), nil
}

// ConfigPath is agy's own, real, unconfigurable MCP config file.
func ConfigPath() (string, error) {
	home, err := geminiHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config", "mcp_config.json"), nil
}

// mcpServer is the subset of agy's own per-entry schema this package reads.
// Confirmed live: {"command": "...", "args": [...], "disabled": false} -
// command and args are separate JSON fields, never one concatenated string.
type mcpServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type mcpConfig struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

// readConfig reads agy's config file. A missing file is not an error: it
// reports existed=false so callers can tell "no entries" from "no file".
func readConfig(path string) (cfg mcpConfig, existed bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return mcpConfig{}, false, nil
	}
	if err != nil {
		return mcpConfig{}, false, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return mcpConfig{}, true, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, true, nil
}

// isTrivialEmptyShell reports whether data is exactly what `agy mcp remove`
// leaves behind on a machine that never had the file before -
// {"mcpServers": {}} - checked structurally (key set and emptiness), not by
// byte comparison, so formatting differences don't matter.
func isTrivialEmptyShell(data []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || len(top) != 1 {
		return false
	}
	raw, ok := top["mcpServers"]
	if !ok {
		return false
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return false
	}
	return len(servers) == 0
}

// recordOriginalExistence writes, once ever, whether agy's config file
// existed before relay first touched it on this machine. It is a no-op if
// the marker already exists - the fact is only ever true as of the very
// first launch, so later launches must never overwrite it.
func recordOriginalExistence(paths relayhome.Paths, existed bool) error {
	p := paths.AgyOriginalSnapshotPath()
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	data, err := json.Marshal(struct {
		Existed bool `json:"existed"`
	}{existed})
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// originalExisted reports the recorded fact, defaulting to true (never
// delete) if no marker was ever written - the conservative direction, since
// the only thing a wrong "true" costs is a harmless leftover empty shell,
// while a wrong "false" could destroy a real user file.
func originalExisted(paths relayhome.Paths) bool {
	data, err := os.ReadFile(paths.AgyOriginalSnapshotPath())
	if err != nil {
		return true
	}
	var v struct {
		Existed bool `json:"existed"`
	}
	if json.Unmarshal(data, &v) != nil {
		return true
	}
	return v.Existed
}

// restoreLocked deletes agy's config file only when both hold: (a) it never
// existed before relay touched this machine, and (b) its current content,
// right now, is exactly the trivial empty shell - i.e. nothing else has put
// anything real in it since. This can never destroy a sibling agent's live
// entry or an unrelated user file: the worst case is a harmless empty shell
// surviving one cycle longer than ideal. Must be called with the lock held.
func restoreLocked(paths relayhome.Paths, cfgPath string) {
	if originalExisted(paths) {
		return
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	if isTrivialEmptyShell(data) {
		_ = os.Remove(cfgPath)
	}
}

func runAgy(agyBin string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, agyBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", agyBin, args, err, out)
	}
	return nil
}

// sweepLocked removes every relay-*-prefixed entry whose owning run
// directory no longer exists (the directory embedded in the entry's own
// registered --dir argument, self-contained, no extra bookkeeping needed).
// This is hygiene for the one case signal-based Cleanup cannot reach
// (`kill -9` of relay itself): a stale entry is already dead on arrival
// (its RelayExe/RunDir target is gone), so leaving it a while longer is not
// itself a risk. Must be called with the lock already held.
func sweepLocked(agyBin, cfgPath string) []string {
	cfg, existed, err := readConfig(cfgPath)
	if !existed || err != nil {
		return nil
	}
	var removed []string
	for name, srv := range cfg.MCPServers {
		if !strings.HasPrefix(name, entryPrefix) {
			continue // never touch a bare "relay" entry (another adaptor's naming) or anything not ours
		}
		dir := dirArg(srv.Args)
		if dir == "" {
			continue
		}
		if _, err := os.Stat(dir); err == nil {
			continue // still owned by a live launch
		}
		if runAgy(agyBin, "mcp", "remove", name) == nil {
			removed = append(removed, name)
		}
	}
	return removed
}

// dirArg extracts the value following a "--dir" argument, matching exactly
// how relay's own adaptors register: ["mcp", "--dir", "<run dir>"].
func dirArg(args []string) string {
	for i, a := range args {
		if a == "--dir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// AddAndSweep registers entry (see EntryName) as relay's MCP server for this
// one launch, first sweeping away any stale relay-* entries left by a prior
// crashed launch. agyBin is the exact, already-resolved agy binary this
// launch is about to run; relayExe/runDir describe the MCP server command
// (`relayExe mcp --dir runDir`, identical in shape to every other adaptor).
func AddAndSweep(paths relayhome.Paths, agyBin, relayExe, runDir string) (entry string, removedStale []string, err error) {
	lock, err := agyFlock(paths.AgyMCPLockPath(), lockWait)
	if err != nil {
		return "", nil, err
	}
	defer lock.Close()

	cfgPath, err := ConfigPath()
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return "", nil, err
	}
	_, existedBefore, err := readConfig(cfgPath)
	if err != nil {
		return "", nil, err
	}
	if err := recordOriginalExistence(paths, existedBefore); err != nil {
		return "", nil, err
	}

	removedStale = sweepLocked(agyBin, cfgPath)

	entry = EntryName(runDir)
	if err := runAgy(agyBin, "mcp", "add", entry, relayExe, "mcp", "--dir", runDir); err != nil {
		return "", removedStale, err
	}
	return entry, removedStale, nil
}

// Remove unregisters entry on exit and, only when provably safe, restores
// true nonexistence (see restoreLocked). Safe to call for an entry that was
// never added - `agy mcp remove` on an unknown name is a harmless no-op-ish
// error, deliberately ignored here so Cleanup never fails on this path.
func Remove(paths relayhome.Paths, agyBin, entry string) error {
	lock, err := agyFlock(paths.AgyMCPLockPath(), lockWait)
	if err != nil {
		return err
	}
	defer lock.Close()

	cfgPath, err := ConfigPath()
	if err != nil {
		return err
	}
	_ = runAgy(agyBin, "mcp", "remove", entry)
	restoreLocked(paths, cfgPath)
	return nil
}

// SweepStale is sweepLocked exposed for `relay gc`, so a user gets a
// one-command cleanup of leftover relay-* entries even if they never
// relaunch agy on this machine.
func SweepStale(paths relayhome.Paths, agyBin string) ([]string, error) {
	lock, err := agyFlock(paths.AgyMCPLockPath(), lockWait)
	if err != nil {
		return nil, err
	}
	defer lock.Close()

	cfgPath, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	return sweepLocked(agyBin, cfgPath), nil
}
