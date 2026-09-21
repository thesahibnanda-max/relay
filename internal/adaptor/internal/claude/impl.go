package internalclaude

import (
	"encoding/json"
	"fmt"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
	"github.com/thesahibnanda-max/relay/internal/hooks"
	"github.com/thesahibnanda-max/relay/internal/state"
)

type ClaudeAdaptor struct{}

func (c *ClaudeAdaptor) Name() string { return "claude" }

func (c *ClaudeAdaptor) Binary() string { return "claude" }

func (c *ClaudeAdaptor) Env() []string { return nil }

// nonInteractive: flags and subcommands where there is no interactive session
// to collaborate in (or nothing to hand our flags to), so the tool runs untouched.
var (
	nonInteractiveFlags = []string{"-p", "--print", "-h", "--help", "-v", "--version", "--bg", "--background"}
	subcommands         = []string{"agents", "attach", "auth", "auto-mode", "doctor", "gateway", "import", "install", "logs", "mcp",
		"plugin", "plugins", "project", "respawn", "rm", "setup-token", "stop", "kill", "ultrareview", "update", "upgrade"}
)

// Prepare adds, for this launch only:
//
//	--mcp-config <run dir>/mcp.json        registers the relay MCP server (additive: the user's own servers stay)
//	--allowedTools mcp__relay              the relay_* tools need no permission prompt
//	--settings <run dir>/settings.json     Relay's hooks (Claude merges them with the user's own)
//	--append-system-prompt <briefing>      protocol + role text (appended to Claude's own prompt)
//
// The variadic flags come first and the single-valued one last, so a prompt
// the user passed positionally is never swallowed as a flag value.
func (c *ClaudeAdaptor) Prepare(spec launch.Spec) (launch.Plan, error) {
	if launch.HasAnyArg(spec.UserArgs, nonInteractiveFlags...) || launch.HasWord(spec.UserArgs, subcommands...) {
		return launch.Passthrough(spec, "not an interactive session: run unchanged"), nil
	}
	plan := launch.Plan{}
	var args []string
	if spec.WithMCP {
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"relay": map[string]any{
			"type": "stdio", "command": spec.RelayExe, "args": []string{"mcp", "--dir", spec.RunDir},
		}}})
		path, err := launch.WriteFile(spec.RunDir, "mcp.json", cfg)
		if err != nil {
			return launch.Plan{}, fmt.Errorf("write MCP config: %w", err)
		}
		args = append(args, "--mcp-config", path, "--allowedTools", "mcp__relay")
		plan.MCP = true
	}
	if spec.WithHooks {
		if launch.HasAnyArg(spec.UserArgs, "--settings") {
			plan.Notes = append(plan.Notes, "you passed --settings yourself; relay's hooks are off, so messages arrive only when the tool is idle")
		} else {
			path, err := launch.WriteFile(spec.RunDir, "settings.json", hooks.SettingsJSON(spec.RelayExe, spec.RunDir))
			if err != nil {
				return launch.Plan{}, fmt.Errorf("write hook settings: %w", err)
			}
			args = append(args, "--settings", path)
			plan.Hooks = true
		}
	}
	if spec.Briefing != "" {
		if launch.HasAnyArg(spec.UserArgs, "--append-system-prompt") {
			plan.Notes = append(plan.Notes, "you passed --append-system-prompt yourself; the relay briefing is typed as a first message instead")
		} else {
			args = append(args, "--append-system-prompt", spec.Briefing)
			plan.BriefingDelivered = true
		}
	}
	plan.Args = append(args, spec.UserArgs...)
	return plan, nil
}

// ScreenRules recognise the dialogs in which Relay must never type.
func (c *ClaudeAdaptor) ScreenRules() []state.ScreenRule {
	return []state.ScreenRule{
		{State: state.Dialog, Contains: "Do you want to", Tail: 14},
		{State: state.Dialog, Contains: "Would you like to proceed", Tail: 14},
		{State: state.Dialog, Contains: "Do you trust the files", Tail: 14},
		{State: state.Dialog, Contains: "Enter to select", Tail: 8},
		{State: state.Dialog, Contains: "Esc to cancel", Tail: 6},
	}
}
