// Package ctl is the private channel between a running agent and the MCP shim
// its tool spawns (`relay mcp`): one unix socket per agent, inside that
// agent's 0700 run directory, guarded by the owner-uid check and a random
// token file. Requests and replies are single JSON lines, one exchange per
// connection.
package ctl

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/peercred"
	"github.com/thesahibnanda-max/relay/internal/proto"
)

const (
	sockName  = "ctl.sock"
	tokenName = "ctl.token"
	maxLine   = 256 << 10
	maxConns  = 32
)

type Request struct {
	Token string          `json:"token"`
	Op    string          `json:"op"`
	Args  json.RawMessage `json:"args,omitempty"`
}

type Response struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *proto.Error    `json:"error,omitempty"`
	Pending int             `json:"pending,omitempty"` // messages waiting for this agent (hint for the model)
}

// Handler serves one operation.
type Handler func(ctx context.Context, op string, args json.RawMessage) (result any, pending int, err *proto.Error)

// Server is a running control socket.
type Server struct {
	ln    net.Listener
	token string
	h     Handler
	wg    sync.WaitGroup
	quit  chan struct{}
	slots chan struct{} // bounds concurrent requests
}

// Serve listens on dir/ctl.sock and writes dir/ctl.token. Stop it with Close.
func Serve(dir string, h Handler) (*Server, error) {
	token := ids.Token()
	if err := os.WriteFile(filepath.Join(dir, tokenName), []byte(token), 0o600); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, sockName)
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(sock, 0o600)
	s := &Server{ln: ln, token: token, h: h, quit: make(chan struct{}), slots: make(chan struct{}, maxConns)}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		if uid, err := peercred.UID(c); err == nil && uid != uint32(os.Getuid()) {
			c.Close()
			continue
		}
		select {
		case s.slots <- struct{}{}:
		default:
			c.Close() // a runaway client cannot exhaust the agent
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.slots }()
			s.serve(c)
		}()
	}
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // stop working for a caller that went away
		select {
		case <-s.quit:
		case <-ctx.Done():
		}
		cancel()
		c.Close()
	}()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReaderSize(c, 64<<10).ReadBytes('\n')
	if err != nil || len(line) > maxLine {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	var req Request
	resp := Response{}
	switch {
	case json.Unmarshal(line, &req) != nil:
		resp.Error = &proto.Error{Code: proto.CodeBadRequest, Message: "malformed request"}
	case subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1:
		resp.Error = &proto.Error{Code: proto.CodeForbidden, Message: "bad token"}
	default:
		out, pending, perr := s.h(ctx, req.Op, req.Args)
		resp.Pending = pending
		if perr != nil {
			resp.Error = perr
		} else {
			resp.OK = true
			if out != nil {
				resp.Result, _ = json.Marshal(out)
			}
		}
	}
	b, _ := json.Marshal(resp)
	_, _ = c.Write(append(b, '\n'))
}

// Close stops accepting, cancels in-flight requests and removes the socket.
func (s *Server) Close() {
	close(s.quit)
	s.ln.Close()
	s.wg.Wait()
}

// Call performs one operation against the agent whose run directory is dir.
// A refusal by the agent or daemon is returned as *proto.Error.
func Call(ctx context.Context, dir, op string, args, out any) (pending int, err error) {
	tok, err := os.ReadFile(filepath.Join(dir, tokenName))
	if err != nil {
		return 0, fmt.Errorf("relay control channel unavailable (%v): is this tool running under `relay`?", err)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return 0, err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", filepath.Join(dir, sockName))
	if err != nil {
		return 0, fmt.Errorf("relay agent is not reachable: %w", err)
	}
	defer c.Close()
	go func() { <-ctx.Done(); c.Close() }()
	b, _ := json.Marshal(Request{Token: strings.TrimSpace(string(tok)), Op: op, Args: raw})
	if _, err := c.Write(append(b, '\n')); err != nil {
		return 0, err
	}
	line, err := bufio.NewReaderSize(c, 64<<10).ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("relay agent closed the connection: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return 0, errors.New("malformed reply from relay agent")
	}
	if resp.Error != nil {
		return resp.Pending, resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return resp.Pending, err
		}
	}
	return resp.Pending, nil
}
