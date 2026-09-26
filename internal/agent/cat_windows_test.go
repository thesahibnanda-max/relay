//go:build windows

package agent

// catCmd is a portable "echo stdin to stdout, exit at EOF" command:
// findstr.exe in regex mode ("^", start-of-line, matches every line) reads
// stdin and echoes it, invoked directly rather than through cmd.exe /c to
// avoid a second, cmd.exe-internal round of command-line parsing re-eating
// the "^" (confirmed live: passed through cmd.exe /c, the caret is either
// consumed by cmd's own escaping ("Bad command line") or, quoted, becomes a
// literal search string instead of the regex anchor). `more`, the other
// obvious candidate, was tried and rejected: confirmed live that under a
// ConPTY (unlike a plain redirected pipe) it waits interactively rather than
// passing input straight through, hanging the test.
func catCmd() (bin string, args []string) { return "findstr.exe", []string{"/r", "^"} }
