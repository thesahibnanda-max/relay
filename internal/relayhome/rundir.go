package relayhome

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thesahibnanda-max/relay/internal/ids"
)

// Per-agent run directories hold the ephemeral files Relay hands to a wrapped
// tool for exactly one launch (MCP config, prompt file, control socket). They
// are the only place Relay writes on the tool's behalf: nothing in the tool's
// own config or the user's project is ever touched. The directory is removed
// when the agent exits; GC removes ones left behind by a crash.

const (
	agentInfoFile = "agent.json"
	tempPrefix    = "relay-run-"
)

// RunInfo identifies who owns a run directory.
type RunInfo struct {
	AgentID string    `json:"agent_id"`
	PID     int       `json:"pid"`
	Root    string    `json:"root"`
	Started time.Time `json:"started"`
}

// AgentDir is where an agent's ephemeral files live: ~/.relay/run/<agent id>,
// or, when that would make the control socket path too long for the OS, a
// short private directory under the temp dir.
func (p Paths) AgentDir(agentID string) string {
	d := filepath.Join(p.RunDir(), agentID)
	if len(filepath.Join(d, "ctl.sock")) <= maxSocketPath {
		return d
	}
	sum := sha256.Sum256([]byte(p.Root + "|" + agentID))
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s%d-%s", tempPrefix, os.Getuid(), hex.EncodeToString(sum[:6])))
}

// CreateAgentDir makes the run directory (0700) and records its owner.
func (p Paths) CreateAgentDir(agentID string) (string, error) {
	d := p.AgentDir(agentID)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	if err := VerifyPrivateDir(d); err != nil {
		return "", err
	}
	info, _ := json.Marshal(RunInfo{AgentID: agentID, PID: os.Getpid(), Root: p.Root, Started: time.Now()})
	if err := os.WriteFile(filepath.Join(d, agentInfoFile), info, 0o600); err != nil {
		os.RemoveAll(d)
		return "", err
	}
	return d, nil
}

// StaleRunDirs lists run directories whose owner is gone, without removing them.
func (p Paths) StaleRunDirs() []string {
	var out []string
	p.gc(PIDAlive, func(d string) { out = append(out, d) }, false)
	return out
}

// GC removes run directories whose owning process is gone and returns their paths.
func (p Paths) GC(alive func(pid int) bool) ([]string, error) {
	if alive == nil {
		alive = PIDAlive
	}
	var removed []string
	p.gc(alive, func(d string) { removed = append(removed, d) }, true)
	return removed, nil
}

func (p Paths) gc(alive func(pid int) bool, found func(dir string), remove bool) {
	var candidates []string
	if entries, err := os.ReadDir(p.RunDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() && ids.Valid(e.Name()) {
				candidates = append(candidates, filepath.Join(p.RunDir(), e.Name()))
			}
		}
	}
	if entries, err := os.ReadDir(os.TempDir()); err == nil {
		prefix := fmt.Sprintf("%s%d-", tempPrefix, os.Getuid())
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
				candidates = append(candidates, filepath.Join(os.TempDir(), e.Name()))
			}
		}
	}
	for _, d := range candidates {
		data, err := os.ReadFile(filepath.Join(d, agentInfoFile))
		var info RunInfo
		if err != nil || json.Unmarshal(data, &info) != nil {
			// No owner record: only treat it as stale once it is clearly not being created right now.
			if st, serr := os.Stat(d); serr != nil || time.Since(st.ModTime()) < time.Minute {
				continue
			}
		} else if info.Root != p.Root || alive(info.PID) {
			continue // someone else's tree, or a live agent
		}
		if remove {
			if err := os.RemoveAll(d); err != nil {
				continue
			}
		}
		found(d)
	}
}

// VerifyPrivateDir makes sure d is a real directory (not a symlink) owned by
// the current user and closed to everyone else, tightening the mode if it is
// ours. Directories under a shared temp dir have predictable names, so another
// user could have pre-created one; using it would hand them our control socket.
func VerifyPrivateDir(d string) error {
	st, err := os.Lstat(d)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory (symlink?): refusing to use it", d)
	}
	if !OwnedByCurrentUser(d, st) {
		return fmt.Errorf("%s belongs to another user: refusing to use it", d)
	}
	return hardenMode(d, st)
}
