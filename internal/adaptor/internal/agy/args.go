package agy

import "strings"

// agy parses its command line with Go's flag package: flags come first (one
// or two dashes, "-f v" or "-f=v"), flag parsing stops at the first
// non-flag argument or "--", and that argument is a subcommand if it names
// one. Which flags take a value was confirmed live against agy 1.2.12/1.2.13
// (`agy <flag>=x changelog`: booleans reject "x").
var valueFlags = map[string]bool{
	"p": true, "print": true, "prompt": true, "i": true, "prompt-interactive": true,
	"conversation": true, "add-dir": true, "agent": true, "effort": true, "input-format": true,
	"json-schema": true, "log-file": true, "mode": true, "model": true, "output-format": true,
	"print-timeout": true, "project": true, "v": true,
}

var subcommands = map[string]bool{
	"agent": true, "agents": true, "changelog": true, "help": true, "install": true, "mcp": true,
	"mic-serve": true, "models": true, "plugin": true, "plugins": true, "remote-control": true, "update": true,
}

// argv is agy's command line as agy itself will read it.
type argv struct {
	subcommand  string // first positional argument, if it names a subcommand
	print       bool   // -p/--print/--prompt: one non-interactive turn
	help        bool   // -h/--help/--version: prints and exits
	resume      bool   // -c/--continue/--conversation: an existing conversation
	interactive string // value of -i/--prompt-interactive
	hasPromptI  bool
	promptIAt   int    // index of the -i flag in the original args (-1 if absent)
	promptIForm int    // 1: "-i v" (value at promptIAt+1), 2: "-i=v"
	logFile     string // value of --log-file
}

func parseArgv(args []string) argv {
	a := argv{promptIAt: -1}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if subcommands[arg] {
				a.subcommand = arg
			}
			break // a positional argument ends flag parsing
		}
		name := strings.TrimLeft(arg, "-")
		value, hasValue := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, value, hasValue = k, v, true
		}
		form := 2
		if valueFlags[name] && !hasValue && i+1 < len(args) {
			i++
			value, hasValue, form = args[i], true, 1
		}
		switch name {
		case "p", "print", "prompt":
			a.print = true
		case "h", "help", "version":
			a.help = !hasValue || value != "false"
		case "c", "continue":
			a.resume = !hasValue || value != "false"
		case "conversation":
			a.resume = true
		case "log-file":
			a.logFile = value
		case "i", "prompt-interactive":
			a.hasPromptI, a.interactive, a.promptIForm = true, value, form
			a.promptIAt = i
			if form == 1 {
				a.promptIAt = i - 1
			}
		}
	}
	return a
}

// nonInteractive reports whether agy will not run an interactive session.
func (a argv) nonInteractive() bool { return a.subcommand != "" || a.print || a.help }

// withBriefing returns args with the briefing delivered as agy's first turn
// through -i. A user's own -i prompt is kept, after the briefing, in the same
// first turn.
func withBriefing(args []string, briefing string) []string {
	a := parseArgv(args)
	out := append([]string(nil), args...)
	if !a.hasPromptI {
		return append([]string{"-i", briefing}, out...)
	}
	combined := briefing
	if strings.TrimSpace(a.interactive) != "" {
		combined += "\n\n---\n\n" + a.interactive
	}
	if a.promptIForm == 1 {
		out[a.promptIAt+1] = combined
	} else {
		flag, _, _ := strings.Cut(out[a.promptIAt], "=")
		out[a.promptIAt] = flag + "=" + combined
	}
	return out
}
