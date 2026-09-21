// Package launch holds what an adaptor needs to know and produce when it
// prepares ONE launch of a wrapped tool.
//
// Zero-footprint rule: everything an adaptor adds is per-launch. It may add
// command-line flags and environment for the child process and write files
// inside Spec.RunDir (deleted when the agent exits). It must never write to
// the tool's own config (~/.claude, ~/.codex, ...) or the user's project, and
// it must never run the tool's "add"/"enable" style commands.
package launch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Spec describes the agent being launched.
type Spec struct {
	AgentName string
	Session   string
	RunDir    string // private, ephemeral, owned by this launch
	RelayExe  string // absolute path of this relay binary (the MCP server command)
	// Briefing is the collaboration protocol plus role text for the model ("" = none).
	Briefing string
	// WithMCP exposes the relay_* tools (only in a shared session).
	WithMCP bool
	// WithHooks registers Relay's hooks for this launch (tools that have them).
	WithHooks bool
	UserArgs  []string // everything after `--`, passed through verbatim
}

// Plan is the result: how to start the tool.
type Plan struct {
	Args []string // complete argv (after the binary) to run
	// BriefingDelivered is true when Briefing reached the tool through a
	// launch flag. If false and there is a briefing, the agent types it once
	// as a bootstrap message when the tool first becomes ready.
	BriefingDelivered bool
	MCP               bool     // relay_* tools are registered for this launch
	Hooks             bool     // Relay's hooks are registered for this launch
	Passthrough       bool     // not an interactive session: the tool runs exactly as typed
	Notes             []string // why anything was skipped or degraded
}

// Passthrough is a Plan that runs the tool exactly as the user asked.
func Passthrough(spec Spec, why string) Plan {
	p := Plan{Args: append([]string(nil), spec.UserArgs...), Passthrough: true}
	if why != "" {
		p.Notes = append(p.Notes, why)
	}
	return p
}

// HasAnyArg reports whether any argument equals (or is --name=value of) one of names.
func HasAnyArg(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n || strings.HasPrefix(a, n+"=") {
				return true
			}
		}
	}
	return false
}

// HasWord reports whether any argument is exactly one of words (subcommand detection).
func HasWord(args []string, words ...string) bool {
	for _, a := range args {
		for _, w := range words {
			if a == w {
				return true
			}
		}
	}
	return false
}

// WriteFile writes a private file inside the run directory.
func WriteFile(dir, name string, data []byte) (string, error) {
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, data, 0o600)
}

var tomlEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)

// TOMLString renders s as a TOML basic string (for `codex -c key=value`).
func TOMLString(s string) string {
	s = tomlEscaper.Replace(s)
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteString(`\u00`)
			b.WriteByte("0123456789ABCDEF"[r>>4])
			b.WriteByte("0123456789ABCDEF"[r&0xf])
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// TOMLStringArray renders a TOML array of basic strings.
func TOMLStringArray(items ...string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = TOMLString(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

var topLevelKey = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_.-]+)\s*=`)

// ConfigDefinesKey reports whether a TOML file sets key at top level (before
// any [table]). It reads the file only; a missing file means "no".
func ConfigDefinesKey(path, key string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	text := string(data)
	if i := strings.Index(text, "\n["); i >= 0 {
		text = text[:i]
	} else if strings.HasPrefix(strings.TrimSpace(text), "[") {
		return false
	}
	for _, m := range topLevelKey.FindAllStringSubmatch(text, -1) {
		if m[1] == key {
			return true
		}
	}
	return false
}
