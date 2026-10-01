package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func geminiDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeagy: no home directory")
		os.Exit(1)
	}
	return filepath.Join(home, ".gemini")
}

func configPath() string { return filepath.Join(geminiDir(), "config", "mcp_config.json") }

// readConfig mirrors agy: missing/empty/whitespace = empty; invalid = error.
func readConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("failed to parse %s: failed to parse JSON config: %v", path, err)
	}
	if top == nil {
		top = map[string]any{}
	}
	return top, nil
}

func servers(top map[string]any) map[string]any {
	s, _ := top["mcpServers"].(map[string]any)
	if s == nil {
		s = map[string]any{}
	}
	return s
}

func writeConfig(path string, top map[string]any) error {
	data, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func mcpCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agy mcp <subcommand> [flags] [args]")
		return 2
	}
	path := configPath()
	top, err := readConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	switch args[0] {
	case "add":
		rest := args[1:]
		env := map[string]any{}
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") && rest[0] != "--" {
			switch rest[0] {
			case "-e", "--env":
				if len(rest) < 2 {
					fmt.Fprintln(os.Stderr, "Error: flag needs an argument")
					return 2
				}
				k, v, _ := strings.Cut(rest[1], "=")
				env[k] = v
				rest = rest[2:]
			default:
				fmt.Fprintf(os.Stderr, "Error: unknown flag %s\n", rest[0])
				return 2
			}
		}
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "Error: agy mcp add requires <name> <commandOrUrl>")
			return 2
		}
		name, cmdArgs := rest[0], rest[1:]
		if cmdArgs[0] == "--" {
			cmdArgs = cmdArgs[1:]
		}
		if len(cmdArgs) > 1 && cmdArgs[1] == "--" {
			cmdArgs = append([]string{cmdArgs[0]}, cmdArgs[2:]...)
		}
		entry := map[string]any{"command": cmdArgs[0], "disabled": false}
		if len(cmdArgs) > 1 {
			list := make([]any, len(cmdArgs)-1)
			for i, a := range cmdArgs[1:] {
				list[i] = a
			}
			entry["args"] = list
		}
		if len(env) > 0 {
			entry["env"] = env
		}
		delay()
		s := servers(top)
		s[name] = entry
		top["mcpServers"] = s
		if err := writeConfig(path, top); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			return 1
		}
		fmt.Printf("Added MCP server %q (stdio)\n", name)
	case "remove":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Error: agy mcp remove requires <name>")
			return 2
		}
		s := servers(top)
		if _, ok := s[args[1]]; !ok {
			fmt.Fprintf(os.Stderr, "Error: MCP server %q not found\n", args[1])
			return 1
		}
		delay()
		delete(s, args[1])
		top["mcpServers"] = s
		if err := writeConfig(path, top); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			return 1
		}
		fmt.Printf("Removed MCP server %q\n", args[1])
	case "list":
		s := servers(top)
		if len(s) == 0 {
			fmt.Println("No MCP servers configured.")
			return 0
		}
		var names []string
		for n := range s {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Println(n)
		}
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown mcp subcommand %q\n", args[0])
		return 2
	}
	return 0
}
