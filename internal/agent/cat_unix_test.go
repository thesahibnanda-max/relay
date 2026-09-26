//go:build unix

package agent

// catCmd is a portable "echo stdin to stdout, exit at EOF" command.
func catCmd() (bin string, args []string) { return "cat", nil }
