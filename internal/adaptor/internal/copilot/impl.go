package internalcopilot

import (
	"encoding/json"
	"fmt"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
	"github.com/thesahibnanda-max/relay/internal/state"
)

type CopilotAdaptor struct{}

func (c *CopilotAdaptor) Name() string { return "copilot" }

func (c *CopilotAdaptor) Binary() string { return "copilot" }

// Deliberately nil: --allow-all/COPILOT_ALLOW_ALL bypasses every permission
// dialog. That is useful only for this adaptor's own automated tests, never
// for real launches - Relay's whole purpose is human-in-the-loop
// orchestration, not bypassing the human.
func (c *CopilotAdaptor) Env() []string { return nil }

// Anything that is not an interactive session (or resumes one, which is
// fine) runs untouched. Verified live against copilot 1.0.88's own --help.
var (
	nonInteractiveFlags = []string{"-p", "--prompt", "-h", "--help", "-v", "--version"}
	subcommands         = []string{"app", "login", "help", "init", "update", "version", "sessions", "memories",
		"plugin", "mcp", "skill", "instruction", "lsp", "completion"}
)

// Prepare adds, for this launch only:
//
//	--additional-mcp-config @<run dir>/mcp.json   registers the relay MCP server (additive: the user's own config stays)
//	--allow-tool=relay                            the relay_* tools need no permission prompt
//
// The mcp.json entry's own "tools":["*"] only controls which tools the model
// is OFFERED, not whether calling one prompts for approval - confirmed live:
// without --allow-tool, every relay_send/relay_whoami call popped "Do you
// want to use this tool?", which would make Relay unusable for real
// collaboration. --allow-tool=relay (the bare MCP server name) approves
// every tool from that one server, mirroring Claude's --allowedTools
// mcp__relay and Codex's default_tools_approval_mode="approve".
//
// Copilot has no CLI flag for a system prompt or instructions override at
// all (only on-disk instruction files, or a full --agent=<file>.agent.md
// definition - neither fits a per-launch, zero-footprint registration). So,
// unlike Claude/Codex, the briefing is NEVER delivered via a flag here: this
// is a permanent design choice, not a conflict-driven degrade. Whenever
// there is a briefing, BriefingDelivered stays false and a note is added, so
// agentcmd.go always types it as a bootstrap message once the tool is ready.
func (c *CopilotAdaptor) Prepare(spec launch.Spec) (launch.Plan, error) {
	if launch.HasAnyArg(spec.UserArgs, nonInteractiveFlags...) || launch.HasWord(spec.UserArgs, subcommands...) {
		return launch.Passthrough(spec, "not an interactive session: run unchanged"), nil
	}
	plan := launch.Plan{}
	var args []string
	if spec.WithMCP {
		if launch.HasAnyArg(spec.UserArgs, "--additional-mcp-config") {
			plan.Notes = append(plan.Notes, "you passed --additional-mcp-config yourself; the relay MCP tools are not registered for this launch")
		} else {
			cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"relay": map[string]any{
				"type": "local", "command": spec.RelayExe, "args": []string{"mcp", "--dir", spec.RunDir}, "tools": []string{"*"},
			}}})
			path, err := launch.WriteFile(spec.RunDir, "mcp.json", cfg)
			if err != nil {
				return launch.Plan{}, fmt.Errorf("write MCP config: %w", err)
			}
			args = append(args, "--additional-mcp-config", "@"+path, "--allow-tool=relay")
			plan.MCP = true
		}
	}
	if spec.Briefing != "" {
		plan.Notes = append(plan.Notes, "copilot has no system-prompt flag; the relay briefing is typed as a first message instead")
	}
	plan.Args = append(args, spec.UserArgs...)
	return plan, nil
}

// ScreenRules recognise the dialogs in which Relay must never type.
//
// Captured live from a real, authenticated copilot 1.0.88 session in an
// isolated tmux server (this project's established verification method):
// the tool-permission prompt ("Do you want to run this command?" / "...tell
// Copilot what to do differently"), the first-run folder-trust prompt
// ("Do you trust the files in this folder?"), and the model's own ask_user
// tool ("Copilot needs information." / "ctrl+d decline") - a separate
// mechanism from permission prompts, discovered live: a real session hit it
// unprompted, asking what to work on, with its own free-text input box.
// These are a backstop: permission.requested/tool.execution_start("ask_user")
// in the transcript (see transcript.CopilotParser) are the primary,
// structured signal, since they are faster and do not depend on exact screen
// wording surviving a CLI upgrade.
func (c *CopilotAdaptor) ScreenRules() []state.ScreenRule {
	return []state.ScreenRule{
		{State: state.Dialog, Contains: "Do you want to run this command", Tail: 14},
		{State: state.Dialog, Contains: "tell Copilot what to do differently", Tail: 14},
		{State: state.Dialog, Contains: "Do you trust the files in this folder", Tail: 14},
		{State: state.Dialog, Contains: "Copilot needs information", Tail: 14},
		{State: state.Dialog, Contains: "ctrl+d decline", Tail: 6},
		{State: state.Dialog, Contains: "esc to cancel", Tail: 6},
	}
}
