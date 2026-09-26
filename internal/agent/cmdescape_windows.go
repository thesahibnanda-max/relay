//go:build windows

package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// IsBatchFile reports whether path is a Windows batch file (.cmd/.bat,
// case-insensitive) - npm's global install of a JS-based CLI tool (codex,
// claude, copilot) drops exactly this kind of file on Windows. Unlike a real
// PE (.exe), a batch file cannot be launched directly by CreateProcess: the
// OS routes it through cmd.exe, whose own command-line tokenizer treats
// `| & < > ^ %` as live metacharacters - a normal argv (which go-pty's own
// CreateProcess-based launch already handles correctly for a real .exe) does
// not protect against this. See WrapForCmdExe.
func IsBatchFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".cmd" || ext == ".bat"
}

// isLocalCmdShim matches cross-spawn's own isCmdShimRegExp
// (github.com/moxystudio/node-cross-spawn, lib/parse.js): a cmd-shim living
// under a project-local node_modules/.bin/ re-runs its own %* through
// cmd.exe a second time internally, so its arguments need the metacharacter
// escaping applied twice. A globally-installed tool's shim (the normal case
// for codex/claude/copilot) does not match this and is escaped once.
var isLocalCmdShim = regexp.MustCompile(`(?i)node_modules[\\/]\.bin[\\/][^\\/]+\.cmd$`)

// cmdMetaChars are the characters cmd.exe treats specially, taken verbatim
// from cross-spawn's own metaCharsRegExp: ( ) ] [ % ! ^ " ` < > & | ; , space * ?
const cmdMetaChars = "()][%!^\"`<>&|;, *?"

// escapeCmdMetaChars caret-escapes every cmd.exe metacharacter in s.
func escapeCmdMetaChars(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(cmdMetaChars, c) >= 0 {
			b.WriteByte('^')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// escapeCmdCommand is cross-spawn's escape.command: caret-escape metachars,
// no quoting - used for the resolved binary path, the first word of the
// composed cmd.exe command line.
func escapeCmdCommand(bin string) string {
	return escapeCmdMetaChars(bin)
}

// escapeCmdArgument is cross-spawn's escape.argument
// (lib/util/escape.js), ported faithfully - the algorithm is
// https://qntm.org/cmd, cross-spawn's own cited source: double every run of
// backslashes immediately before a double quote (and any trailing run at the
// end of the string, since a closing quote follows), backslash-escape the
// quote itself, wrap the whole argument in quotes, then caret-escape cmd.exe's
// metacharacters - twice when doubleEscapeMetaChars is set (see isLocalCmdShim).
func escapeCmdArgument(arg string, doubleEscapeMetaChars bool) string {
	var b strings.Builder
	b.Grow(len(arg) + 8)
	backslashes := 0
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		switch c {
		case '\\':
			backslashes++
			b.WriteByte(c)
		case '"':
			for ; backslashes > 0; backslashes-- {
				b.WriteByte('\\')
			}
			b.WriteByte('\\')
			b.WriteByte('"')
		default:
			backslashes = 0
			b.WriteByte(c)
		}
	}
	// A trailing run of backslashes would otherwise escape the closing quote
	// we're about to add.
	for ; backslashes > 0; backslashes-- {
		b.WriteByte('\\')
	}
	quoted := `"` + b.String() + `"`
	escaped := escapeCmdMetaChars(quoted)
	if doubleEscapeMetaChars {
		escaped = escapeCmdMetaChars(escaped)
	}
	return escaped
}

// comspec returns cmd.exe's real path, matching cross-spawn's own fallback.
func comspec() string {
	if c := os.Getenv("ComSpec"); c != "" {
		return c
	}
	return `C:\Windows\System32\cmd.exe`
}

// WrapForCmdExe builds the cmd.exe invocation needed to safely launch bin (a
// .cmd/.bat file, confirmed via IsBatchFile by the caller) with args that may
// contain arbitrary content, including cmd.exe metacharacters - see the
// IsBatchFile doc comment for why a plain argv (correct for a real .exe)
// cannot do this safely. cmdExe is the real binary to launch (CreateProcess's
// lpApplicationName, via go-pty's Cmd.Path); cmdLine is the fully pre-escaped
// command line to pass verbatim via Cmd.SysProcAttr.CmdLine, which go-pty's
// Cmd.start honours in preference to composing one from Cmd.Args (confirmed
// by reading cmd_windows.go) - the direct equivalent of cross-spawn setting
// Node's windowsVerbatimArguments: true once it has done its own escaping.
func WrapForCmdExe(bin string, args []string) (cmdExe, cmdLine string) {
	doubleEscape := isLocalCmdShim.MatchString(bin)
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, escapeCmdCommand(filepath.Clean(bin)))
	for _, a := range args {
		parts = append(parts, escapeCmdArgument(a, doubleEscape))
	}
	shellCommand := strings.Join(parts, " ")
	cmdExe = comspec()
	cmdLine = `"` + cmdExe + `" /d /s /c "` + shellCommand + `"`
	return cmdExe, cmdLine
}
