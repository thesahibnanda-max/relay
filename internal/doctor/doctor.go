// Package doctor diagnoses a Relay installation: permissions, the daemon, the
// database, leftovers, the wrapped tools, and (the zero-footprint promise)
// whether anything of Relay's has ended up outside ~/.relay where it does not
// belong.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/thesahibnanda-max/relay/internal/adaptor"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
	"github.com/thesahibnanda-max/relay/internal/store"
)

type Status int

const (
	OK   Status = iota
	Info        // worth knowing, nothing to do
	Warn        // should be looked at
	Fail        // broken or unsafe
)

func (s Status) String() string { return [...]string{"ok", "info", "warn", "FAIL"}[s] }

type Check struct {
	Name   string
	Status Status
	Detail string
	Fix    string // what to do about it
}

// Env is everything the checks read from the outside world; tests replace it.
type Env struct {
	Paths       relayhome.Paths
	Home        string // the user's home directory
	Cwd         string
	GOOS        string
	Getenv      func(string) string
	DaemonQuery func(relayhome.Paths) (*proto.Status, error)
	ToolVersion func(binary string) (path, version string, err error)
	Version     string
}

// DefaultEnv is the real environment.
func DefaultEnv(paths relayhome.Paths, version string, query func(relayhome.Paths) (*proto.Status, error)) Env {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	return Env{Paths: paths, Home: home, Cwd: cwd, GOOS: runtime.GOOS, Getenv: os.Getenv, DaemonQuery: query, ToolVersion: toolVersion, Version: version}
}

func toolVersion(binary string) (string, string, error) {
	path, err := adaptor.ResolveBinary(binary)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return path, "", nil // present but would not say
	}
	return path, strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), nil
}

// Run performs every check.
func Run(env Env) []Check {
	var out []Check
	out = append(out, checkPlatform(env)...)
	out = append(out, checkHome(env)...)
	out = append(out, checkDaemon(env))
	out = append(out, checkStore(env)...)
	out = append(out, checkLeftovers(env))
	out = append(out, checkTools(env)...)
	out = append(out, checkFootprint(env)...)
	return out
}

// Worst is the most severe status among the checks.
func Worst(cs []Check) Status {
	w := OK
	for _, c := range cs {
		if c.Status > w {
			w = c.Status
		}
	}
	return w
}

// Render prints the checks and a summary line.
func Render(w io.Writer, cs []Check) {
	mark := map[Status]string{OK: " ok ", Info: "info", Warn: "warn", Fail: "FAIL"}
	for _, c := range cs {
		fmt.Fprintf(w, "[%s] %-26s %s\n", mark[c.Status], c.Name, c.Detail)
		if c.Fix != "" && c.Status >= Warn {
			fmt.Fprintf(w, "       %-26s -> %s\n", "", c.Fix)
		}
	}
	var counts [4]int
	for _, c := range cs {
		counts[c.Status]++
	}
	fmt.Fprintf(w, "\n%d ok, %d info, %d warning(s), %d failure(s)\n", counts[OK], counts[Info], counts[Warn], counts[Fail])
}

// ---- platform ----------------------------------------------------------------

func checkPlatform(env Env) []Check {
	var out []Check
	switch env.GOOS {
	case "linux", "darwin":
		out = append(out, Check{Name: "platform", Status: OK, Detail: env.GOOS})
	default:
		out = append(out, Check{Name: "platform", Status: Fail, Detail: env.GOOS + " is not supported", Fix: "use Linux, macOS or WSL"})
	}
	if env.Getenv("RELAY_ACTIVE") != "" {
		out = append(out, Check{Name: "nesting", Status: Info, Detail: "running inside a relay-wrapped tool (RELAY_ACTIVE is set)"})
	}
	// SQLite's WAL mode needs real file locking, which Windows drives mounted in WSL do not give.
	if strings.HasPrefix(env.Paths.Root, "/mnt/") && len(env.Paths.Root) > 6 && env.Paths.Root[6] == '/' {
		out = append(out, Check{Name: "storage location", Status: Warn, Detail: env.Paths.Root + " is on a Windows drive: SQLite locking is unreliable there and it is slow",
			Fix: "keep RELAY_HOME on the Linux filesystem (the default ~/.relay is fine)"})
	}
	return out
}

// ---- home directory ------------------------------------------------------------

func mode(path string) (fs.FileMode, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return st.Mode().Perm(), nil
}

func checkHome(env Env) []Check {
	p := env.Paths
	if _, err := os.Stat(p.Root); err != nil {
		return []Check{{Name: "relay home", Status: Info, Detail: p.Root + " does not exist yet (created on first use)"}}
	}
	var bad []string
	for _, d := range []string{p.Root, p.RunDir(), p.DataDir(), p.RawDir(), p.LogDir(), p.SessionsDir()} {
		if m, err := mode(d); err == nil && m&0o077 != 0 {
			bad = append(bad, fmt.Sprintf("%s is %#o", d, m))
		}
		if st, err := os.Lstat(d); err == nil {
			if !relayhome.OwnedByCurrentUser(st) {
				bad = append(bad, d+" belongs to another user")
			}
		}
	}
	for _, f := range []string{p.DBPath(), p.DaemonLog()} {
		if m, err := mode(f); err == nil && m&0o077 != 0 {
			bad = append(bad, fmt.Sprintf("%s is %#o", f, m))
		}
	}
	if m, err := mode(p.SocketPath()); err == nil && m&0o077 != 0 {
		bad = append(bad, fmt.Sprintf("%s is %#o", p.SocketPath(), m))
	}
	if len(bad) > 0 {
		return []Check{{Name: "relay home", Status: Fail, Detail: "other users could read Relay's data: " + strings.Join(bad, "; "),
			Fix: "chmod -R go-rwx " + p.Root + "  (relay repairs directories itself on next start)"}}
	}
	return []Check{{Name: "relay home", Status: OK, Detail: p.Root + " (private)"}}
}

// ---- daemon --------------------------------------------------------------------

func checkDaemon(env Env) Check {
	st, err := env.DaemonQuery(env.Paths)
	if err != nil {
		return Check{Name: "daemon", Status: Info, Detail: "not running (starts on demand)"}
	}
	up := time.Since(st.StartedAt).Round(time.Second)
	if st.Proto != proto.Version {
		return Check{Name: "daemon", Status: Fail, Detail: fmt.Sprintf("running (pid %d) but speaks protocol v%d; this relay speaks v%d", st.PID, st.Proto, proto.Version),
			Fix: "relay daemon stop   (the next relay command starts a matching one)"}
	}
	d := fmt.Sprintf("running: pid %d, version %s, up %s, %d session(s), %d agent(s) connected", st.PID, st.Version, up, st.Sessions, st.Agents)
	if env.Version != "" && st.Version != env.Version {
		return Check{Name: "daemon", Status: Warn, Detail: d + fmt.Sprintf("; this binary is %s", env.Version), Fix: "relay daemon stop   (restarts on the new version when no agent needs it)"}
	}
	return Check{Name: "daemon", Status: OK, Detail: d}
}

// ---- store -----------------------------------------------------------------------

func dirBytes(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if i, err := d.Info(); err == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}

func human(n int64) string {
	const k = 1024
	switch {
	case n < k*k:
		return fmt.Sprintf("%.0f KiB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.2f GiB", float64(n)/(k*k*k))
}

func checkStore(env Env) []Check {
	db := env.Paths.DBPath()
	if _, err := os.Stat(db); err != nil {
		return nil // nothing recorded yet
	}
	var out []Check
	res, err := store.QuickCheck(db)
	switch {
	case err != nil:
		out = append(out, Check{Name: "database", Status: Warn, Detail: "cannot check " + db + ": " + err.Error()})
	case res != "ok":
		out = append(out, Check{Name: "database", Status: Fail, Detail: "integrity check failed: " + res, Fix: "stop the daemon and restore or move " + db + " aside (a fresh one is created)"})
	default:
		v, _ := store.StoredSchemaVersion(db)
		switch {
		case v > store.SchemaVersion():
			out = append(out, Check{Name: "database", Status: Fail, Detail: fmt.Sprintf("schema v%d is newer than this relay understands (v%d)", v, store.SchemaVersion()), Fix: "upgrade relay"})
		default:
			out = append(out, Check{Name: "database", Status: OK, Detail: fmt.Sprintf("integrity ok, schema v%d", v)})
		}
	}
	total := dirBytes(env.Paths.DataDir()) + dirBytes(env.Paths.SessionsDir())
	c := Check{Name: "disk use", Status: OK, Detail: human(total) + " of recorded sessions"}
	if total > 2<<30 {
		c.Status, c.Fix = Warn, "relay gc --older-than=30d --compress"
	}
	return append(out, c)
}

func checkLeftovers(env Env) Check {
	stale := env.Paths.StaleRunDirs()
	if len(stale) == 0 {
		return Check{Name: "leftovers", Status: OK, Detail: "no stale per-launch files"}
	}
	return Check{Name: "leftovers", Status: Warn, Detail: fmt.Sprintf("%d run directory(ies) left by crashed agents", len(stale)), Fix: "relay gc"}
}

// ---- tools -------------------------------------------------------------------------

func checkTools(env Env) []Check {
	var out []Check
	f := adaptor.NewAdaptorFactory()
	for _, name := range f.Names() {
		a, _ := f.ByName(name)
		path, ver, err := env.ToolVersion(a.Binary())
		if err != nil {
			out = append(out, Check{Name: "tool: " + name, Status: Info, Detail: "not found on PATH (relay " + name + " will not work until it is installed)"})
			continue
		}
		d := path
		if ver != "" {
			d += " (" + ver + ")"
		}
		out = append(out, Check{Name: "tool: " + name, Status: OK, Detail: d})
	}
	return out
}

// ---- zero footprint --------------------------------------------------------------------

// The strings Relay's registrations contain. Relay only ever passes these on
// a command line for one launch; finding one in a config file means something
// (not Relay) wrote it.
var footprintMarks = []string{"relay mcp --dir", "relay hook ", "mcp__relay", "mcp_servers.relay", "Relay session (id", "relay_send"}

func checkFootprint(env Env) []Check {
	claudeHome := env.Home + "/.claude"
	codexHome := env.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = env.Home + "/.codex"
	}
	files := []string{
		claudeHome + "/settings.json", claudeHome + "/settings.local.json", claudeHome + "/CLAUDE.md", env.Home + "/.claude.json",
		codexHome + "/config.toml", codexHome + "/AGENTS.md",
		env.Cwd + "/.mcp.json", env.Cwd + "/.claude/settings.json", env.Cwd + "/.claude/settings.local.json",
		env.Cwd + "/CLAUDE.md", env.Cwd + "/AGENTS.md", env.Cwd + "/.codex/config.toml",
	}
	var found []string
	scanned := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		scanned++
		text := string(data)
		for _, m := range footprintMarks {
			if strings.Contains(text, m) {
				found = append(found, fmt.Sprintf("%s mentions %q", f, m))
				break
			}
		}
		if strings.HasSuffix(f, ".json") && hasRelayMCPServer(data) && !strings.Contains(strings.Join(found, ";"), f) {
			found = append(found, fmt.Sprintf("%s registers an MCP server named \"relay\"", f))
		}
	}
	if len(found) > 0 {
		return []Check{{Name: "zero footprint", Status: Warn, Detail: strings.Join(found, "; "),
			Fix: "Relay never writes these files; remove the entry (it may have been added by hand or by another tool) so plain claude/codex behave as before"}}
	}
	return []Check{{Name: "zero footprint", Status: OK, Detail: fmt.Sprintf("no Relay registrations in the %d tool config/project files checked", scanned)}}
}

// hasRelayMCPServer reports whether a JSON document has an mcpServers table
// with an entry called "relay" at any depth (~/.claude.json keeps them per project).
func hasRelayMCPServer(data []byte) bool {
	var v any
	if json.Unmarshal(data, &v) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(n any) bool {
		switch x := n.(type) {
		case map[string]any:
			if m, ok := x["mcpServers"].(map[string]any); ok {
				if _, has := m["relay"]; has {
					return true
				}
			}
			for _, c := range x {
				if walk(c) {
					return true
				}
			}
		case []any:
			for _, c := range x {
				if walk(c) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}
