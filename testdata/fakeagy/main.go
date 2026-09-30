// fakeagy is a scripted stand-in for the real `agy` binary (Google's
// Antigravity CLI), faithful to what was confirmed live against agy
// 1.2.12-1.2.14, so the agy adaptor can be tested end to end without a real,
// authenticated agy or a real ~/.gemini:
//
//   - `mcp add/remove/list` (mcpcmd.go) edit $HOME/.gemini/config/mcp_config.json
//     exactly like agy: flags before the name, "--" before the command, add
//     or update, Go-sorted 2-space output, an empty or missing file is empty,
//     invalid JSON is a hard error, unknown fields survive, and removing the
//     last entry leaves {"mcpServers": {}}. Like agy it does NO locking of
//     its own (a real, confirmed agy bug: concurrent adds lose updates).
//   - Without a subcommand it is an interactive TUI (tui.go): bracketed
//     paste, a prompt that Enter submits, queued messages while busy,
//     permission dialogs, the folder-trust prompt, -i, -c, /new, Ctrl+D
//     twice to exit - writing a conversation database (db.go) with agy's
//     real schema and step/status/executor_metadata sequence, and spawning
//     every configured MCP server as its own child, as agy does.
//
// Environment (all optional):
//
//	FAKEAGY_DELAY_MS       sleep between reading and writing the MCP config
//	                       (widens the race window for concurrency tests)
//	FAKEAGY_LOG            append one JSON line per event (ready, submit,
//	                       queued, dialog, answer, mcp) to this file
//	FAKEAGY_LOGIN_MS       act as if logging in for this long at startup
//	                       (input is queued, as agy does)
//	FAKEAGY_TRUST          "prompt": show the folder-trust dialog first
//	FAKEAGY_NO_MCP         "1": do not spawn MCP servers
//	FAKEAGY_TURN_MS        how long each model step takes (default 150)
//	FAKEAGY_ALLOW_MCP      "1": never ask before an MCP tool call
//	FAKEAGY_SWALLOW_ENTER  ignore this many Enters that follow a paste (the
//	                       "typed but never submitted" symptom of issue #50)
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func delay() {
	if v, err := strconv.Atoi(os.Getenv("FAKEAGY_DELAY_MS")); err == nil && v > 0 {
		time.Sleep(time.Duration(v) * time.Millisecond)
	}
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "mcp" {
		os.Exit(mcpCommand(args[1:]))
	}
	if len(args) > 0 && (args[0] == "--version" || args[0] == "-version") {
		fmt.Println(version())
		return
	}
	os.Exit(runTUI(args))
}

// version is the agy version faked (FAKEAGY_VERSION overrides it).
func version() string {
	if v := os.Getenv("FAKEAGY_VERSION"); v != "" {
		return v
	}
	return "1.2.14"
}
