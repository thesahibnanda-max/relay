package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// mcpServer is one stdio MCP server agy spawned at startup, as agy does for
// every entry in its config: its own child process, inheriting agy's
// environment (confirmed live), handshaking server/discover, then
// initialize (protocol 2025-11-25), then tools/list.
type mcpServer struct {
	name  string
	cmd   *exec.Cmd
	in    io.WriteCloser
	mu    sync.Mutex
	next  int
	wait  map[int]chan map[string]any
	tools []string
	dead  bool
}

func startMCPServers() map[string]*mcpServer {
	out := map[string]*mcpServer{}
	if os.Getenv("FAKEAGY_NO_MCP") == "1" {
		return out
	}
	top, err := readConfig(configPath())
	if err != nil {
		return out
	}
	for name, v := range servers(top) {
		e, _ := v.(map[string]any)
		command, _ := e["command"].(string)
		if command == "" {
			continue
		}
		if d, _ := e["disabled"].(bool); d {
			continue
		}
		var args []string
		if list, ok := e["args"].([]any); ok {
			for _, a := range list {
				if s, ok := a.(string); ok {
					args = append(args, s)
				}
			}
		}
		s := &mcpServer{name: name, wait: map[int]chan map[string]any{}}
		s.cmd = exec.Command(command, args...)
		s.cmd.Env = os.Environ()
		if env, ok := e["env"].(map[string]any); ok {
			for k, v := range env {
				s.cmd.Env = append(s.cmd.Env, fmt.Sprintf("%s=%v", k, v))
			}
		}
		s.cmd.Stderr = io.Discard
		stdin, err1 := s.cmd.StdinPipe()
		stdout, err2 := s.cmd.StdoutPipe()
		if err1 != nil || err2 != nil || s.cmd.Start() != nil {
			continue
		}
		s.in = stdin
		go s.readLoop(stdout)
		s.request("server/discover", map[string]any{})
		s.request("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "antigravity-client", "version": "v1.0.0"}})
		s.notify("notifications/initialized")
		if res, ok := s.request("tools/list", map[string]any{}); ok {
			if list, ok := res["tools"].([]any); ok {
				for _, t := range list {
					if m, ok := t.(map[string]any); ok {
						if n, ok := m["name"].(string); ok {
							s.tools = append(s.tools, n)
						}
					}
				}
			}
		}
		sort.Strings(s.tools)
		out[name] = s
	}
	return out
}

func (s *mcpServer) readLoop(r io.Reader) {
	defer func() { // the server went away: nobody waits for it any more
		s.mu.Lock()
		for id, ch := range s.wait {
			close(ch)
			delete(s.wait, id)
		}
		s.dead = true
		s.mu.Unlock()
	}()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		id, ok := m["id"].(float64)
		if !ok {
			continue
		}
		s.mu.Lock()
		ch := s.wait[int(id)]
		delete(s.wait, int(id))
		s.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

func (s *mcpServer) notify(method string) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": map[string]any{}})
	s.in.Write(append(b, '\n'))
}

// request sends one JSON-RPC request and waits for its result.
func (s *mcpServer) request(method string, params any) (map[string]any, bool) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return nil, false
	}
	s.next++
	id := s.next
	ch := make(chan map[string]any, 1)
	s.wait[id] = ch
	s.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		return nil, false
	}
	select {
	case m, open := <-ch:
		if !open {
			return nil, false
		}
		res, ok := m["result"].(map[string]any)
		return res, ok
	case <-time.After(60 * time.Second):
		return nil, false
	}
}

// call runs a tool and returns its text result.
func (s *mcpServer) call(tool string, args map[string]any) (string, bool) {
	res, ok := s.request("tools/call", map[string]any{"name": tool, "arguments": args})
	if !ok {
		return "no result", true
	}
	text := ""
	if content, ok := res["content"].([]any); ok && len(content) > 0 {
		if m, ok := content[0].(map[string]any); ok {
			text, _ = m["text"].(string)
		}
	}
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func (s *mcpServer) stop() {
	if s.in != nil {
		s.in.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		done := make(chan struct{})
		go func() { s.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			s.cmd.Process.Kill()
		}
	}
}
