package agy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
	"github.com/thesahibnanda-max/relay/internal/state"
)

// markerFile records, inside the launch's own RunDir, which MCP entry (if
// any) Prepare registered - so Cleanup can find it later with no
// adaptor-struct state, and so Cleanup is a safe no-op when Prepare never
// registered anything (passthrough, or a failed registration).
const markerFile = "agy-mcp-entry"

type AgyAdaptor struct{}

func (a *AgyAdaptor) Name() string { return "agy" }

func (a *AgyAdaptor) Binary() string { return "agy" }

// Deliberately nil: --dangerously-skip-permissions bypasses every permission
// dialog, matching Copilot's own --allow-all precedent - useful only for
// this adaptor's own automated tests, never for a real launch.
func (a *AgyAdaptor) Env() []string { return nil }

// Anything that is not an interactive session (or resumes one, which is
// fine) runs untouched. Verified live against agy 1.2.11's own --help.
var (
	nonInteractiveFlags = []string{"-h", "--help", "--version", "-p", "--print", "--prompt"}
	subcommands         = []string{"agent", "agents", "changelog", "help", "install", "mcp",
		"mic-serve", "models", "plugin", "plugins", "remote-control", "update"}
)

// Prepare registers relay's MCP server as a uniquely-named entry
// ("relay-<run dir base>", never the bare "relay" every other adaptor uses -
// see EntryName) in agy's own global, persistent mcp_config.json, since agy
// has no per-invocation registration flag at all. This is the one adaptor
// that cannot satisfy relay's zero-footprint rule outright; mcp.go's
// locking, staleness sweep, and existence-only restoration exist specifically
// to make that tradeoff safe under crashes and concurrent agy agents. A
// marker recording the registered entry is written to spec.RunDir only on
// confirmed success, so Cleanup can reverse exactly this launch's own change.
//
// agy has no confirmed system-prompt flag: the briefing is always typed as a
// first message instead, matching Copilot's own precedent.
func (a *AgyAdaptor) Prepare(spec launch.Spec) (launch.Plan, error) {
	if launch.HasAnyArg(spec.UserArgs, nonInteractiveFlags...) || launch.HasWord(spec.UserArgs, subcommands...) {
		return launch.Passthrough(spec, "not an interactive session: run unchanged"), nil
	}
	plan := launch.Plan{}
	if spec.WithMCP {
		paths := relayhome.Paths{Root: spec.RelayHome}
		entry, removedStale, err := AddAndSweep(paths, spec.ToolBin, spec.RelayExe, spec.RunDir)
		if err != nil {
			plan.Notes = append(plan.Notes, fmt.Sprintf("could not register the relay MCP server with agy: %v", err))
		} else if _, werr := launch.WriteFile(spec.RunDir, markerFile, []byte(entry)); werr != nil {
			// No marker means Cleanup can never find this entry: undo the add
			// right now rather than leave a half-registered, untracked entry.
			_ = Remove(paths, spec.ToolBin, entry)
			plan.Notes = append(plan.Notes, fmt.Sprintf("could not register the relay MCP server with agy: %v", werr))
		} else {
			plan.MCP = true
			if len(removedStale) > 0 {
				plan.Notes = append(plan.Notes, fmt.Sprintf("removed %d stale agy MCP registration(s) left by a previous crash", len(removedStale)))
			}
		}
	}
	if spec.Briefing != "" {
		plan.Notes = append(plan.Notes, "agy has no system-prompt flag; the relay briefing is typed as a first message instead")
	}
	plan.Args = spec.UserArgs
	return plan, nil
}

// Cleanup unregisters whatever entry Prepare registered for this launch, if
// any. It is always safe to call, including when Prepare was never called,
// failed, or registered nothing (no marker file: a no-op) - see the Adaptor
// interface's doc comment for the reliability profile this must meet.
func (a *AgyAdaptor) Cleanup(spec launch.Spec) error {
	data, err := os.ReadFile(filepath.Join(spec.RunDir, markerFile))
	if err != nil {
		return nil // nothing was registered for this launch
	}
	entry := strings.TrimSpace(string(data))
	if entry == "" {
		return nil
	}
	paths := relayhome.Paths{Root: spec.RelayHome}
	return Remove(paths, spec.ToolBin, entry)
}

// ScreenRules recognise the dialogs in which Relay must never type.
//
// Captured live from a real, authenticated agy session (this project's
// established verification method): the tool-permission prompt ("Requesting
// permission for:" / "Run this command?") and the first-run folder-trust
// prompt ("Accessing workspace:" / "Do you trust the contents of this
// project?" / "Yes, I trust this folder").
func (a *AgyAdaptor) ScreenRules() []state.ScreenRule {
	return []state.ScreenRule{
		{State: state.Dialog, Contains: "Requesting permission for", Tail: 14},
		{State: state.Dialog, Contains: "Run this command?", Tail: 14},
		{State: state.Dialog, Contains: "Accessing workspace", Tail: 14},
		{State: state.Dialog, Contains: "Do you trust the contents of this project", Tail: 14},
		{State: state.Dialog, Contains: "Yes, I trust this folder", Tail: 6},
	}
}
