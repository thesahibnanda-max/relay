package agy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
	"github.com/thesahibnanda-max/relay/internal/state"
	"github.com/thesahibnanda-max/relay/internal/transcript"
)

// markerFile records, inside the launch's own RunDir, that Prepare holds a
// registration lease - so Cleanup is a safe no-op when Prepare never
// registered anything (passthrough, or a failed registration).
const markerFile = "agy-mcp-lease"

type AgyAdaptor struct{}

func (a *AgyAdaptor) Name() string { return "agy" }

func (a *AgyAdaptor) Binary() string { return "agy" }

// TestedVersion reports whether the agy that printed version (its --version
// output) is one relay was verified against, and which ones those are. An
// unparseable version is not flagged.
func (a *AgyAdaptor) TestedVersion(version string) (ok bool, tested string) {
	tested = transcript.AgyTestedMin + " to " + transcript.AgyTestedMax
	v := versionRe.FindString(version)
	return v == "" || transcript.AgyVersionTested(v), tested
}

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// EOFPresses: agy exits only on a second Ctrl+D ("press ctrl+d again to
// exit", confirmed live), so piped stdin ending sends two.
func (a *AgyAdaptor) EOFPresses() int { return 2 }

// Deliberately nil: --dangerously-skip-permissions bypasses every permission
// dialog, matching Copilot's own --allow-all precedent - useful only for
// this adaptor's own automated tests, never for a real launch.
func (a *AgyAdaptor) Env() []string { return nil }

func options(spec launch.Spec) Options {
	return Options{
		AgyBin: spec.ToolBin, RelayExe: spec.RelayExe,
		LegacyMarker: relayhome.Paths{Root: spec.RelayHome}.AgyOriginalSnapshotPath(),
	}
}

// Prepare registers relay's MCP server with agy (see mcp.go: one stable,
// environment-scoped entry in agy's user-global config, since agy has no
// per-invocation registration flag at all) and delivers the briefing as the
// session's real first turn through -i, which agy runs as soon as it is
// ready - after login and the folder-trust prompt - instead of relay typing
// it at a moment agy may not accept input.
func (a *AgyAdaptor) Prepare(spec launch.Spec) (launch.Plan, error) {
	av := parseArgv(spec.UserArgs)
	if av.nonInteractive() {
		return launch.Passthrough(spec, "not an interactive session: run unchanged"), nil
	}
	plan := launch.Plan{Args: append([]string(nil), spec.UserArgs...)}
	if spec.WithMCP {
		notes, err := Register(options(spec), spec.AgentID)
		plan.Notes = append(plan.Notes, notes...)
		switch {
		case err != nil:
			plan.Notes = append(plan.Notes, fmt.Sprintf("could not register the relay MCP server with agy: %v", err))
		default:
			if _, werr := launch.WriteFile(spec.RunDir, markerFile, []byte(spec.AgentID)); werr != nil {
				_ = Unregister(options(spec), spec.AgentID)
				plan.Notes = append(plan.Notes, fmt.Sprintf("could not register the relay MCP server with agy: %v", werr))
				break
			}
			plan.MCP = true
			// The server finds this launch's run directory here (see
			// `relay mcp --from-env`); RELAY_AGENT_ID is already set.
			plan.Env = append(plan.Env, "RELAY_RUN_DIR="+spec.RunDir)
			if !permissionGranted() {
				plan.Notes = append(plan.Notes, `agy asks before every MCP tool call; to let this agent use relay's tools without a prompt each time, allow them once: pick "always allow ... (Persist to settings.json)" at the first prompt, or add "mcp(relay/*)" to permissions.allow in ~/.gemini/antigravity-cli/settings.json`)
			}
		}
	}
	if spec.RunDir != "" {
		// agy's own log names the conversation this process is on and every
		// prompt it accepts (see collab's followAgy). Each launch gets its
		// own, handed back to agy's log directory on exit; a user's own
		// --log-file is simply read instead.
		plan.ToolLog = av.logFile
		if plan.ToolLog == "" {
			plan.ToolLog = filepath.Join(spec.RunDir, logFile)
			plan.Args = append([]string{"--log-file", plan.ToolLog}, plan.Args...)
		}
		plan.VerifySubmit = true
		// Confirmed live (1.2.14): prompts typed while agy is still signing
		// in are each put in a conversation of their own before the -i
		// briefing runs. Wait for agy's database to report that first turn done.
		plan.StartupGate = spec.WithMCP
	}
	if spec.Briefing != "" {
		plan.Args = withBriefing(plan.Args, spec.Briefing)
		plan.BriefingDelivered = true
	}
	return plan, nil
}

// logFile is the per-launch agy log inside the run directory.
const logFile = "agy.log"

// keepLog moves this launch's agy log to where agy keeps its own logs, under
// the name agy would have given it, so nothing the user expects goes missing.
func keepLog(spec launch.Spec) {
	src := filepath.Join(spec.RunDir, logFile)
	st, err := os.Stat(src)
	if err != nil || st.Size() == 0 {
		return
	}
	home, err := geminiHome()
	if err != nil {
		return
	}
	started := st.ModTime()
	if info, err := relayhome.ReadRunInfo(spec.RunDir); err == nil && !info.Started.IsZero() {
		started = info.Started
	}
	dir := filepath.Join(home, "antigravity-cli", "log")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	dst := filepath.Join(dir, "cli-"+started.Format("20060102_150405")+".log")
	if _, err := os.Stat(dst); err == nil {
		dst = filepath.Join(dir, "cli-"+started.Format("20060102_150405")+"-relay-"+spec.AgentID+".log")
	}
	if os.Rename(src, dst) != nil {
		if data, err := os.ReadFile(src); err == nil {
			_ = os.WriteFile(dst, data, 0o644)
		}
	}
}

// permissionGranted reports whether the user already allows relay's tools in
// agy's own settings (read-only; relay never writes that file).
func permissionGranted() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	ok, _ := RelayToolsAllowed(home)
	return ok
}

// PermissionQuestion is how the launch offer starts (tests look for it).
const PermissionQuestion = "relay: agy asks before every relay tool call."

// OfferToolPermission returns the question to ask before an interactive
// launch whose agy settings do not yet allow relay's tools; ok is false when
// there is nothing to offer (already allowed, not interactive, or settings
// relay cannot read - agy's own prompt still works then).
func (a *AgyAdaptor) OfferToolPermission(userArgs []string) (question string, ok bool) {
	if parseArgv(userArgs).nonInteractive() {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	if allowed, err := RelayToolsAllowed(home); err != nil || allowed {
		return "", false
	}
	return fmt.Sprintf("%s Allow them from now on, by adding %q to permissions.allow in %s? [y/N] ", PermissionQuestion, relayToolsRule, settingsPath(home)), true
}

// GrantToolPermission allows relay's tools in agy's settings (the user said
// yes to OfferToolPermission).
func (a *AgyAdaptor) GrantToolPermission() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return AllowRelayTools(home)
}

// Cleanup ends this launch's registration lease; the last relay agy agent to
// exit removes relay's entry and restores agy's config byte-for-byte. It is
// always safe to call, including when Prepare never registered anything.
func (a *AgyAdaptor) Cleanup(spec launch.Spec) error {
	if spec.RunDir != "" {
		keepLog(spec)
	}
	data, err := os.ReadFile(filepath.Join(spec.RunDir, markerFile))
	if err != nil {
		return nil // nothing was registered for this launch
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return nil
	}
	return Unregister(options(spec), id)
}

// ScreenRules recognise the screens in which Relay must never type. Every
// agy prompt that takes a decision (tool, command, file, URL and permission
// approvals, the folder-trust prompt, ask_question, sign-in) draws a
// numbered or arrow-selected menu with an "↑/↓ Navigate" hint just above
// the status line, and Enter picks the highlighted option - so an injected
// message would answer it (confirmed live: it approved a shell command). The
// hint is the one anchor common to all of them; the titles below it are
// kept as a second net for layouts that push the hint out of view. Tool
// permission prompts are also detected authoritatively from agy's
// conversation database (a step waiting for approval).
func (a *AgyAdaptor) ScreenRules() []state.ScreenRule {
	dialog := func(s string, tail int) state.ScreenRule {
		return state.ScreenRule{State: state.Dialog, Contains: s, Tail: tail}
	}
	return []state.ScreenRule{
		dialog("↑/↓ Navigate", 4),
		dialog("Requesting permission for", 40),
		dialog("Run this command?", 40),
		dialog("Allow calling this tool?", 40),
		dialog("Allow creation of this file?", 40),
		dialog("Accept this file edit?", 40),
		dialog("Allow access to this", 40), // "... file?", "... URL?"
		dialog("Grant requested permission?", 40),
		dialog("Approve this action?", 40),
		dialog("Accessing workspace", 40),
		dialog("Do you trust the contents of this project", 40),
		dialog("Select Google Cloud sign-in method", 40),
		dialog("Import this conversation?", 40),
		dialog("Discard changes? (y/n)", 8),
		dialog("press ctrl+d again to exit", 3),
		dialog("press ctrl+c again to exit", 3),
		{State: state.Busy, Contains: "esc to cancel", Tail: 2},
		{State: state.Busy, Contains: "Press up to edit queued messages", Tail: 2},
	}
}
