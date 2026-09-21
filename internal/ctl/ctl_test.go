package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/proto"
)

func short(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestRoundTripPermissionsAndErrors(t *testing.T) {
	dir := short(t)
	srv, err := Serve(dir, func(ctx context.Context, op string, args json.RawMessage) (any, int, *proto.Error) {
		switch op {
		case "echo":
			var a map[string]string
			json.Unmarshal(args, &a)
			return a, 3, nil
		case "fail":
			return nil, 0, &proto.Error{Code: proto.CodeUnknownAgent, Message: "nope", Agents: []proto.PeerInfo{{Name: "bob"}}}
		case "slow":
			<-ctx.Done()
			return nil, 0, nil
		}
		return nil, 0, &proto.Error{Code: proto.CodeBadRequest, Message: "unknown op"}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	for _, f := range []string{sockName, tokenName} {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s must be private: %v %v", f, err, st)
		}
	}
	ctx := context.Background()
	var out map[string]string
	pending, err := Call(ctx, dir, "echo", map[string]string{"a": "b"}, &out)
	if err != nil || out["a"] != "b" || pending != 3 {
		t.Fatalf("echo: %v %v %d", out, err, pending)
	}
	_, err = Call(ctx, dir, "fail", nil, nil)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeUnknownAgent || len(pe.Agents) != 1 {
		t.Fatalf("structured errors must survive: %v", err)
	}

	// a wrong token is refused
	if err := os.WriteFile(filepath.Join(dir, tokenName), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(ctx, dir, "echo", nil, nil); !errors.As(err, &pe) || pe.Code != proto.CodeForbidden {
		t.Fatalf("bad token: %v", err)
	}
	// a cancelled caller frees the handler
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	os.WriteFile(filepath.Join(dir, tokenName), []byte(srv.token), 0o600)
	start := time.Now()
	if _, err := Call(cctx, dir, "slow", nil, nil); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("slow call: %v after %v", err, time.Since(start))
	}
}

func TestCallWithoutAgentExplainsItself(t *testing.T) {
	_, err := Call(context.Background(), short(t), "x", nil, nil)
	if err == nil || !contains(err.Error(), "running under `relay`") {
		t.Fatalf("%v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
