// Package relayhome knows where Relay keeps its files.
//
//	~/.relay/            (override: RELAY_HOME)   0700
//	  run/relayd.sock    daemon socket            0600
//	  run/relayd.lock    single-daemon lock
//	  run/relayd.pid
//	  data/relay.db      SQLite store
//	  data/raw/<session>/<agent>/...   raw terminal stream segments
//	  log/relayd.log
//	  sessions/          agent-side local event logs (the offline spool)
package relayhome

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// maxSocketPath is conservative: sun_path is 108 bytes on Linux, 104 on macOS.
const maxSocketPath = 100

type Paths struct{ Root string }

// Resolve returns the paths rooted at $RELAY_HOME or ~/.relay.
func Resolve() (Paths, error) {
	if h := os.Getenv("RELAY_HOME"); h != "" {
		abs, err := filepath.Abs(h)
		return Paths{Root: abs}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	return Paths{Root: filepath.Join(home, ".relay")}, nil
}

func (p Paths) RunDir() string      { return filepath.Join(p.Root, "run") }
func (p Paths) DataDir() string     { return filepath.Join(p.Root, "data") }
func (p Paths) RawDir() string      { return filepath.Join(p.Root, "data", "raw") }
func (p Paths) LogDir() string      { return filepath.Join(p.Root, "log") }
func (p Paths) SessionsDir() string { return filepath.Join(p.Root, "sessions") }
func (p Paths) DBPath() string      { return filepath.Join(p.DataDir(), "relay.db") }
func (p Paths) LockPath() string    { return filepath.Join(p.RunDir(), "relayd.lock") }
func (p Paths) SpawnLockPath() string {
	return filepath.Join(p.RunDir(), "spawn.lock")
}
func (p Paths) PidPath() string   { return filepath.Join(p.RunDir(), "relayd.pid") }
func (p Paths) DaemonLog() string { return filepath.Join(p.LogDir(), "relayd.log") }

// SocketPath is run/relayd.sock, unless that would exceed the OS limit on
// unix socket paths; then it is a short, stable path under the temp dir
// derived from the root (so every process computes the same one).
func (p Paths) SocketPath() string {
	s := filepath.Join(p.RunDir(), "relayd.sock")
	if len(s) <= maxSocketPath {
		return s
	}
	sum := sha256.Sum256([]byte(p.Root))
	// In its own private directory (created and verified by Ensure): a bare
	// file in a shared temp dir could be squatted by another user.
	return filepath.Join(os.TempDir(), fmt.Sprintf("relay-%d-%s", os.Getuid(), hex.EncodeToString(sum[:4])), "relayd.sock")
}

// Ensure creates the directory tree with private permissions.
func (p Paths) Ensure() error {
	for _, d := range []string{p.Root, p.RunDir(), p.DataDir(), p.RawDir(), p.LogDir(), p.SessionsDir(), filepath.Dir(p.SocketPath())} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		// MkdirAll leaves an existing dir's mode alone; tighten our own tree
		// (and refuse one that is a symlink or belongs to someone else).
		if err := VerifyPrivateDir(d); err != nil {
			return err
		}
	}
	return nil
}
