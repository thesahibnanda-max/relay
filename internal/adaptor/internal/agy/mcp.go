// Package agy wraps Google's Antigravity CLI (the real invocable command is
// `agy`, not the marketing name "antigravity"). Unlike Claude/Codex/Copilot,
// agy has no per-invocation MCP-server-registration flag: the only way to
// register one is `agy mcp add`, which writes into agy's own persistent,
// user-global ~/.gemini/config/mcp_config.json. This file makes registering
// there anyway safe under relay's real, concurrent, crash-prone usage.
//
// Design (every point below was confirmed live against agy 1.2.12/1.2.13):
//
//   - One stable entry, "relay", running `relay mcp --from-env`. Every agy
//     process on the machine spawns every entry in that file, as its own
//     child, with its own environment. So the entry cannot name a run
//     directory; instead the server finds it in RELAY_RUN_DIR and serves
//     tools only when RELAY_AGENT_ID matches that directory's owner - which
//     is only ever true for the agy relay itself launched. An unrelated agy
//     session gets a valid server with no tools. A stable name also keeps
//     agy's per-server state (its tool-schema cache and any `mcp(relay/*)`
//     permission rule the user adds) from multiplying per launch.
//   - Leases: each launch writes a lease (its relay pid) before adding the
//     entry, and removes it on exit. The entry and every trace of relay are
//     removed when the last live lease goes; a lease whose pid is dead
//     (kill -9) is simply ignored, so the next launch or `relay gc` finishes
//     the cleanup.
//   - Byte-exact restore: agy rewrites the whole file on every add/remove
//     (sorted keys, 2-space indent, and {"mcpServers": {}} where there was
//     nothing), so the first launch snapshots the original bytes and the
//     last one writes them back - but only if nothing else changed the file
//     in between (compared semantically); otherwise agy's version stands.
//   - Every read-modify-write of agy's file happens under one per-user lock
//     next to that file (agy itself does no locking: concurrent `agy mcp
//     add` calls lose updates), and the lock and state live there too, so
//     relays with different RELAY_HOMEs still exclude each other.
package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// lockWait bounds how long a launch waits for another relay's agy
// bookkeeping: longer than the worst case of one full Register (a capped
// legacy sweep plus one add, each command bounded by cmdTimeout). A variable
// only so tests can shorten it.
var lockWait = 90 * time.Second

const (
	// ServerName is the one MCP entry relay registers with agy.
	ServerName = "relay"
	cmdTimeout = 15 * time.Second
	// legacyPrefix is the per-launch naming older relay versions used.
	legacyPrefix = "relay-"
	// maxLegacySweep caps legacy removals per lock hold, so a large leftover
	// pile can never starve a launch waiting on the lock; the rest go next time.
	maxLegacySweep = 3
	stateDirName   = ".relay-agy"
)

// serverArgs is the exact argv relay's own entry runs (after the relay
// binary), and how relay recognises the entry as its own.
var serverArgs = []string{"mcp", "--from-env"}

// geminiHome is the directory agy itself uses. agy resolves it from the
// user's home directory and honours no override (confirmed: no GEMINI_HOME in
// the binary), so relay must not honour one either - it would make relay
// bookkeep a different file than the one `agy mcp add` edits.
func geminiHome() (string, error) {
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

// ---- reading agy's config ----------------------------------------------------

// entry is the part of one agy MCP entry relay reads. Decoded leniently:
// agy itself accepts (and preserves) odd shapes such as a string "args", so
// relay must never fail on a user entry it does not own.
type entry struct {
	Command string
	Args    []string
}

type config struct {
	Servers map[string]entry
}

// ErrUnparseable means agy's config is not valid JSON. agy then refuses every
// `agy mcp` command too, so relay leaves the file alone and says why.
var ErrUnparseable = errors.New("agy cannot read its own MCP config")

// readConfig reads agy's config. A missing, empty or whitespace-only file is
// an empty config, exactly as agy treats it (confirmed live).
func readConfig(path string) (cfg config, raw []byte, existed bool, err error) {
	cfg.Servers = map[string]entry{}
	raw, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil, false, nil
	}
	if err != nil {
		return cfg, nil, false, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, raw, true, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return cfg, raw, true, fmt.Errorf("%w (%s: %v); fix or empty that file, then relaunch", ErrUnparseable, path, err)
	}
	var servers map[string]json.RawMessage
	if s, ok := top["mcpServers"]; ok {
		_ = json.Unmarshal(s, &servers) // a non-object here is agy's problem, not ours: treat as no servers
	}
	for name, v := range servers {
		var e struct {
			Command any `json:"command"`
			Args    any `json:"args"`
		}
		_ = json.Unmarshal(v, &e)
		var out entry
		out.Command, _ = e.Command.(string)
		if list, ok := e.Args.([]any); ok {
			for _, a := range list {
				if s, ok := a.(string); ok {
					out.Args = append(out.Args, s)
				}
			}
		}
		cfg.Servers[name] = out
	}
	return cfg, raw, true, nil
}

// isOurs reports whether e is relay's own stable entry.
func isOurs(e entry) bool { return reflect.DeepEqual(e.Args, serverArgs) }

// semantic normalises a config for comparison: empty means {}, and an empty
// mcpServers table means no table (agy writes one where there was nothing).
func semantic(raw []byte) (any, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, true
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["mcpServers"].(map[string]any); ok && len(s) == 0 {
			delete(m, "mcpServers")
		}
	}
	return v, true
}

// ---- running agy -------------------------------------------------------------

func runAgy(agyBin string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, agyBin, args...)
	cmd.WaitDelay = 2 * time.Second // a child holding the pipes open must not hang us past the timeout
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", filepath.Base(agyBin), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---- leases and snapshot -----------------------------------------------------

type lease struct {
	AgentID string    `json:"agent_id"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	// Ident is the process's pid and start time (relayhome.ProcessIdentity):
	// once relay is killed, a process that later gets the same pid does not
	// keep the lease alive. Empty in leases older relays wrote.
	Ident string `json:"ident,omitempty"`
}

type snapshot struct {
	Existed bool   `json:"existed"`
	Data    []byte `json:"data"` // base64 in JSON
	Mode    uint32 `json:"mode"`
}

type regState struct {
	dir      string // relay's lock, leases and snapshot, next to the file they protect
	cfgPath  string
	cacheDir string // where agy caches the tool schemas of ServerName
	agyBin   string
	alive    func(pid int) bool
	ident    func(pid int) string
}

func (s regState) leasePath(agentID string) string {
	return filepath.Join(s.dir, "leases", agentID+".json")
}

func (s regState) snapshotPath() string { return filepath.Join(s.dir, "snapshot.json") }

// liveLeases lists the agent ids holding a lease whose process is alive.
func (s regState) liveLeases() []string {
	entries, err := os.ReadDir(filepath.Join(s.dir, "leases"))
	if err != nil {
		return nil
	}
	var live []string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(s.dir, "leases", e.Name()))
		var l lease
		if err != nil || json.Unmarshal(data, &l) != nil {
			continue
		}
		if s.alive(l.PID) && (l.Ident == "" || s.ident(l.PID) == "" || s.ident(l.PID) == l.Ident) {
			live = append(live, l.AgentID)
		}
	}
	sort.Strings(live)
	return live
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func (s regState) writeLease(agentID string) error {
	if err := os.MkdirAll(filepath.Join(s.dir, "leases"), 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(lease{AgentID: agentID, PID: os.Getpid(), Started: time.Now(), Ident: s.ident(os.Getpid())})
	return writeFileAtomic(s.leasePath(agentID), data, 0o600)
}

// takeSnapshot records agy's config as it is before relay's first add.
// legacyExisted carries the answer an older relay recorded, when it recorded
// that the file did not exist before relay created it.
func (s regState) takeSnapshot(legacyNotExisted bool) error {
	_, raw, existed, err := readConfig(s.cfgPath)
	if err != nil {
		return err
	}
	snap := snapshot{Existed: existed, Data: raw, Mode: 0o644}
	if st, err := os.Stat(s.cfgPath); err == nil {
		snap.Mode = uint32(st.Mode().Perm())
	}
	if existed && legacyNotExisted {
		if v, ok := semantic(raw); ok && reflect.DeepEqual(v, map[string]any{}) {
			snap.Existed = false // an older relay created this empty shell
		}
	}
	data, _ := json.Marshal(snap)
	return writeFileAtomic(s.snapshotPath(), data, 0o600)
}

// restore puts back the snapshot, if nothing but relay changed the file since.
func (s regState) restore() {
	data, err := os.ReadFile(s.snapshotPath())
	if err != nil {
		return
	}
	var snap snapshot
	if json.Unmarshal(data, &snap) != nil {
		return
	}
	cur, err := os.ReadFile(s.cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	want, ok1 := semantic(snap.Data)
	have, ok2 := semantic(cur)
	if !snap.Existed {
		want, ok1 = map[string]any{}, true
	}
	if !ok1 || !ok2 || !reflect.DeepEqual(want, have) {
		return // someone changed the file meanwhile: agy's version stands
	}
	if !snap.Existed {
		_ = os.Remove(s.cfgPath)
		return
	}
	if bytes.Equal(cur, snap.Data) {
		return
	}
	mode := os.FileMode(snap.Mode)
	if mode == 0 {
		mode = 0o644
	}
	_ = writeFileAtomic(s.cfgPath, snap.Data, mode)
}

// finish removes relay's entry, restores the original file and deletes all
// of relay's state. Called with the lock held, only when no lease is live.
func (s regState) finish() (removed []string, err error) {
	cfg, _, _, rerr := readConfig(s.cfgPath)
	if rerr != nil {
		return nil, rerr
	}
	if e, ok := cfg.Servers[ServerName]; ok && isOurs(e) {
		if err := runAgy(s.agyBin, "mcp", "remove", ServerName); err != nil {
			return nil, err
		}
		cfg, _, _, _ = readConfig(s.cfgPath)
		if e, ok := cfg.Servers[ServerName]; ok && isOurs(e) {
			return nil, fmt.Errorf("agy mcp remove %s left the entry in place", ServerName)
		}
		removed = append(removed, ServerName)
	}
	s.restore()
	_ = os.RemoveAll(s.cacheDir)
	_ = os.RemoveAll(filepath.Join(s.dir, "leases"))
	_ = os.Remove(s.snapshotPath())
	return removed, nil
}

// sweepLegacy removes per-launch entries older relay versions registered
// (relay-<id> running `mcp --dir <run dir>`) whose run directory is gone or
// whose owner process is dead, a few per call. Called with the lock held.
func (s regState) sweepLegacy(cfg config) []string {
	var names []string
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	var removed []string
	for _, name := range names {
		if len(removed) >= maxLegacySweep {
			break
		}
		e := cfg.Servers[name]
		if !strings.HasPrefix(name, legacyPrefix) {
			continue
		}
		dir := dirArg(e.Args)
		if dir == "" || len(e.Args) != 3 || e.Args[0] != "mcp" {
			continue // not something relay ever registered
		}
		if _, err := os.Stat(dir); err == nil {
			if info, err := relayhome.ReadRunInfo(dir); err != nil || s.alive(info.PID) {
				continue // still owned by a live (older) relay, or not provably dead
			}
		}
		if runAgy(s.agyBin, "mcp", "remove", name) == nil {
			removed = append(removed, name)
		}
	}
	return removed
}

// dirArg extracts the value following a "--dir" argument.
func dirArg(args []string) string {
	for i, a := range args {
		if a == "--dir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// ---- public API ----------------------------------------------------------------

// Options configures one registration call. Zero values are the real ones.
type Options struct {
	AgyBin   string
	RelayExe string
	// LegacyMarker is an older relay's per-RELAY_HOME existence record; its
	// "did not exist" answer is honoured once and the file removed.
	LegacyMarker string
	// Alive reports whether a pid is running (relayhome.PIDAlive by default).
	Alive func(pid int) bool
	// Ident names a running process (relayhome.ProcessIdentity by default).
	Ident func(pid int) string
	// Home is the user's home directory (default: the real one).
	Home string
}

func (o Options) state() (regState, error) {
	gem := filepath.Join(o.Home, ".gemini")
	if o.Home == "" {
		var err error
		if gem, err = geminiHome(); err != nil {
			return regState{}, err
		}
	}
	alive, ident := o.Alive, o.Ident
	if alive == nil {
		alive = relayhome.PIDAlive
	}
	if ident == nil {
		ident = relayhome.ProcessIdentity
	}
	return regState{
		dir:      filepath.Join(gem, "config", stateDirName),
		cfgPath:  filepath.Join(gem, "config", "mcp_config.json"),
		cacheDir: filepath.Join(gem, "antigravity-cli", "mcp", ServerName),
		agyBin:   o.AgyBin,
		alive:    alive,
		ident:    ident,
	}, nil
}

// withLock runs fn holding the per-user lock, creating the state directory.
// When fn leaves no live lease behind, the state directory - lock included -
// is removed, so no trace of relay remains in agy's config directory.
func withLock(s regState, fn func() error) error {
	lockPath := filepath.Join(s.dir, "lock")
	lk, err := agyFlock(lockPath, func() error { return os.MkdirAll(s.dir, 0o700) }, lockWait)
	if err != nil {
		return err
	}
	ferr := fn()
	idle := len(s.liveLeases()) == 0
	if idle {
		removeLockedFile(lockPath) // Unix: safe while held (acquirers verify the inode)
	}
	lk.Close()
	if idle {
		removeUnlockedFile(lockPath) // Windows: only possible once closed; fails harmlessly if reopened
		_ = os.Remove(filepath.Join(s.dir, "leases"))
		_ = os.Remove(s.dir) // only if empty: a concurrent launch may already be recreating it
	}
	return ferr
}

// Register makes relay's MCP server available to the agy this launch is
// about to start, for agentID. The lease is written before the entry is
// added, so a crash at any point leaves only things the next launch or
// `relay gc` cleans up. notes are for the user.
func Register(o Options, agentID string) (notes []string, err error) {
	if !ids.Valid(agentID) {
		return nil, fmt.Errorf("invalid agent id %q", agentID) // it names a lease file
	}
	s, err := o.state()
	if err != nil {
		return nil, err
	}
	err = withLock(s, func() error {
		cfg, _, _, err := readConfig(s.cfgPath)
		if err != nil {
			return err
		}
		if e, ok := cfg.Servers[ServerName]; ok && !isOurs(e) {
			return fmt.Errorf("agy already has an MCP server named %q that relay did not register (%s); rename or remove it with `agy mcp remove %s`", ServerName, e.Command, ServerName)
		}
		if len(s.liveLeases()) == 0 {
			// First relay agy agent: tidy up after any crashed predecessor,
			// then remember the file as the user had it.
			_, statErr := os.Stat(s.snapshotPath())
			if e, ok := cfg.Servers[ServerName]; statErr == nil || (ok && isOurs(e)) {
				if _, err := s.finish(); err != nil {
					return err
				}
			}
			legacyNotExisted := false
			if o.LegacyMarker != "" {
				if data, err := os.ReadFile(o.LegacyMarker); err == nil {
					var v struct {
						Existed bool `json:"existed"`
					}
					legacyNotExisted = json.Unmarshal(data, &v) == nil && !v.Existed
				}
			}
			if err := s.takeSnapshot(legacyNotExisted); err != nil {
				return err
			}
			if o.LegacyMarker != "" {
				_ = os.Remove(o.LegacyMarker)
			}
			cfg, _, _, _ = readConfig(s.cfgPath)
		}
		if removed := s.sweepLegacy(cfg); len(removed) > 0 {
			notes = append(notes, fmt.Sprintf("removed %d stale agy MCP registration(s) left by an older relay", len(removed)))
		}
		if err := s.writeLease(agentID); err != nil {
			return err
		}
		cur, ok := cfg.Servers[ServerName]
		if ok && cur.Command == o.RelayExe {
			return nil
		}
		if ok && len(s.liveLeases()) > 1 {
			if _, err := os.Stat(cur.Command); err == nil {
				return nil // another live agent's relay binary already serves it
			}
		}
		addArgs := append([]string{"mcp", "add", ServerName, "--", o.RelayExe}, serverArgs...)
		if err := runAgy(s.agyBin, addArgs...); err != nil {
			_ = os.Remove(s.leasePath(agentID))
			return err
		}
		cfg, _, _, err = readConfig(s.cfgPath)
		if err != nil {
			return err
		}
		if e, ok := cfg.Servers[ServerName]; !ok || !isOurs(e) {
			_ = os.Remove(s.leasePath(agentID))
			return fmt.Errorf("agy mcp add %s did not register the entry", ServerName)
		}
		return nil
	})
	return notes, err
}

// Unregister ends agentID's lease and, if it was the last live one, removes
// relay's entry and restores agy's config exactly as the user had it.
func Unregister(o Options, agentID string) error {
	if !ids.Valid(agentID) {
		return fmt.Errorf("invalid agent id %q", agentID)
	}
	s, err := o.state()
	if err != nil {
		return err
	}
	return withLock(s, func() error {
		_ = os.Remove(s.leasePath(agentID))
		if len(s.liveLeases()) > 0 {
			return nil
		}
		_, err := s.finish()
		return err
	})
}

// Sweep is `relay gc`'s cleanup: with no live relay agy agent left, it
// removes relay's entry and state (crash leftovers), plus legacy entries.
func Sweep(o Options) (removed []string, err error) {
	s, err := o.state()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(s.dir); errors.Is(err, os.ErrNotExist) {
		// Nothing of the current scheme; only legacy entries could remain.
		cfg, _, existed, err := readConfig(s.cfgPath)
		if err != nil || !existed || !hasLegacy(cfg) {
			return nil, err
		}
	}
	err = withLock(s, func() error {
		cfg, _, _, err := readConfig(s.cfgPath)
		if err != nil {
			return err
		}
		removed = append(removed, s.sweepLegacy(cfg)...)
		if len(s.liveLeases()) > 0 {
			return nil
		}
		r, err := s.finish()
		removed = append(removed, r...)
		return err
	})
	return removed, err
}

func hasLegacy(cfg config) bool {
	for name, e := range cfg.Servers {
		if strings.HasPrefix(name, legacyPrefix) && dirArg(e.Args) != "" {
			return true
		}
	}
	return false
}

// Status describes relay's registration with agy, for `relay doctor`.
type Status struct {
	ConfigPath string
	Registered bool     // relay's entry is in agy's config
	LiveAgents []string // agent ids of running relay agy agents
	Legacy     []string // older relay's per-launch entries still present
	ParseError error    // agy's config is not valid JSON
}

// Stale reports whether relay's entry is left over with nobody using it.
func (st Status) Stale() bool { return st.Registered && len(st.LiveAgents) == 0 }

// Inspect reports the registration state without changing anything.
func Inspect(o Options) (Status, error) {
	s, err := o.state()
	if err != nil {
		return Status{}, err
	}
	st := Status{ConfigPath: s.cfgPath, LiveAgents: s.liveLeases()}
	cfg, _, _, err := readConfig(s.cfgPath)
	if err != nil {
		st.ParseError = err
		return st, nil
	}
	if e, ok := cfg.Servers[ServerName]; ok && isOurs(e) {
		st.Registered = true
	}
	for name, e := range cfg.Servers {
		if strings.HasPrefix(name, legacyPrefix) && dirArg(e.Args) != "" {
			st.Legacy = append(st.Legacy, name)
		}
	}
	sort.Strings(st.Legacy)
	return st, nil
}
