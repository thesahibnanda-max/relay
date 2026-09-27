// fakeagy is a scripted stand-in for the real `agy` binary's `mcp add`/`mcp
// remove` subcommands, used to test the agy adaptor deterministically without
// touching a real ~/.gemini. It deliberately does a naive, unsynchronized
// read-modify-write of its config file with NO locking of its own - mirroring
// the real, confirmed-live bug in the real agy binary (10 concurrent
// `agy mcp add` calls silently lost one addition) - so a test exercising it
// directly (bypassing relay's own lock) can prove that bug still reproduces,
// and a test going through relay's own locked wrapper can prove the lock
// fixes it.
//
// Config path: $GEMINI_HOME/config/mcp_config.json, matching the real agy
// binary's own resolution (as approximated by this package's geminiHome()).
//
// Environment:
//
//	FAKEAGY_DELAY_MS   sleep this long between reading and writing the config
//	                   file (default 0), widening the race window so a
//	                   concurrency test does not depend on getting lucky with
//	                   real filesystem timing.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type mcpServer struct {
	Command  string   `json:"command"`
	Args     []string `json:"args"`
	Disabled bool     `json:"disabled"`
}

type mcpConfig struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

func configPath() string {
	home := os.Getenv("GEMINI_HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "fakeagy: GEMINI_HOME must be set")
		os.Exit(1)
	}
	return filepath.Join(home, "config", "mcp_config.json")
}

func delay() {
	if v, err := strconv.Atoi(os.Getenv("FAKEAGY_DELAY_MS")); err == nil && v > 0 {
		time.Sleep(time.Duration(v) * time.Millisecond)
	}
}

func readConfig(path string) mcpConfig {
	cfg := mcpConfig{MCPServers: map[string]mcpServer{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]mcpServer{}
	}
	return cfg
}

func writeConfig(path string, cfg mcpConfig) {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, data, 0o600)
}

func main() {
	args := os.Args[1:]
	if len(args) < 1 || args[0] != "mcp" {
		fmt.Fprintln(os.Stderr, "fakeagy: usage: fakeagy mcp <add|remove> ...")
		os.Exit(2)
	}
	path := configPath()
	switch args[1] {
	case "add":
		name, command, rest := args[2], args[3], args[4:]
		cfg := readConfig(path) // read...
		delay()                 // ...(a real race window here, uncorrected: no lock)...
		cfg.MCPServers[name] = mcpServer{Command: command, Args: append([]string{}, rest...), Disabled: false}
		writeConfig(path, cfg) // ...write: last writer wins, silently dropping anyone else's add in between.
		fmt.Printf("Added MCP server %q (stdio)\n", name)
	case "remove":
		name := args[2]
		cfg := readConfig(path)
		delay()
		delete(cfg.MCPServers, name)
		writeConfig(path, cfg)
		fmt.Printf("Removed MCP server %q\n", name)
	default:
		fmt.Fprintln(os.Stderr, "fakeagy: unknown mcp subcommand", args[1])
		os.Exit(2)
	}
}
