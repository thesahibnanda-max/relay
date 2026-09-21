// Relay is a transparent layer between you and your AI coding agents
// (Claude Code, Codex, ...). Run `relay help` for usage.
package main

import (
	"os"

	"github.com/thesahibnanda-max/relay/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args)) }
