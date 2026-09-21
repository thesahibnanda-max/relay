package internalcodex

import (
	"os"
	"path/filepath"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
	"github.com/thesahibnanda-max/relay/internal/state"
)

type CodexAdaptor struct{}

func (c *CodexAdaptor) Name() string { return "codex" }

func (c *CodexAdaptor) Binary() string { return "codex" }

func (c *CodexAdaptor) Env() []string { return nil }

// Anything that is not an interactive session (or resumes one, which is fine)
// runs untouched.
var (
	nonInteractiveFlags = []string{"-h", "--help", "-V", "--version"}
	subcommands         = []string{"agents", "exec", "e", "review", "login", "logout", "mcp", "plugin", "app-server", "remote-control",
		"completion", "update", "doctor", "sandbox", "debug", "apply", "a", "queue", "archive", "delete", "migrate-rollouts",
		"unarchive", "cloud", "exec-server", "features", "help"}
)

func codexConfig() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	return filepath.Join(home, "config.toml")
}

// Prepare adds, for this launch only, `-c` overrides (Codex documents them as
// per-invocation config; nothing is written):
//
//	mcp_servers.relay.*           the relay MCP server, with tool calls auto-approved
//	developer_instructions        protocol + role text
//
// developer_instructions REPLACES the user's own value if their config.toml
// sets one, so in that case the briefing is typed as a first message instead.
func (c *CodexAdaptor) Prepare(spec launch.Spec) (launch.Plan, error) {
	if launch.HasAnyArg(spec.UserArgs, nonInteractiveFlags...) || launch.HasWord(spec.UserArgs, subcommands...) {
		return launch.Passthrough(spec, "not an interactive session: run unchanged"), nil
	}
	plan := launch.Plan{}
	var args []string
	if spec.WithMCP {
		args = append(args,
			"-c", "mcp_servers.relay.command="+launch.TOMLString(spec.RelayExe),
			"-c", "mcp_servers.relay.args="+launch.TOMLStringArray("mcp", "--dir", spec.RunDir),
			"-c", `mcp_servers.relay.default_tools_approval_mode="approve"`,
		)
		plan.MCP = true
	}
	if spec.Briefing != "" {
		switch {
		case launch.HasAnyArg(spec.UserArgs, "-c", "--config") && userSetsInstructions(spec.UserArgs):
			plan.Notes = append(plan.Notes, "you set developer_instructions yourself; the relay briefing is typed as a first message instead")
		case launch.ConfigDefinesKey(codexConfig(), "developer_instructions"):
			plan.Notes = append(plan.Notes, "your Codex config sets developer_instructions; the relay briefing is typed as a first message instead")
		default:
			args = append(args, "-c", "developer_instructions="+launch.TOMLString(spec.Briefing))
			plan.BriefingDelivered = true
		}
	}
	plan.Args = append(args, spec.UserArgs...)
	return plan, nil
}

func userSetsInstructions(args []string) bool {
	for i, a := range args {
		if (a == "-c" || a == "--config") && i+1 < len(args) && hasKeyPrefix(args[i+1], "developer_instructions") {
			return true
		}
		if len(a) > 2 && a[:2] == "-c" && hasKeyPrefix(a[2:], "developer_instructions") {
			return true
		}
	}
	return false
}

func hasKeyPrefix(kv, key string) bool {
	return len(kv) > len(key) && kv[:len(key)] == key && kv[len(key)] == '='
}

// ScreenRules recognise the dialogs in which Relay must never type.
func (c *CodexAdaptor) ScreenRules() []state.ScreenRule {
	return []state.ScreenRule{
		{State: state.Dialog, Contains: "Press enter to confirm or esc to cancel", Tail: 14},
		{State: state.Dialog, Contains: "Would you like to", Tail: 14},
		{State: state.Dialog, Contains: "Implement this plan", Tail: 14},
		{State: state.Dialog, Contains: "Do you trust", Tail: 14},
		{State: state.Dialog, Contains: "esc to cancel", Tail: 6},
	}
}
