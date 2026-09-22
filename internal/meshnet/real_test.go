//go:build mesh_real

// This file exercises the real tailcat transport (real WireGuard tunnel,
// real DERP bootstrap) instead of the in-memory FakeTransport used by every
// other test in this package. It needs real network access and is never run
// by `make check`/`make short`/CI — opt in explicitly:
//
//	go test -tags mesh_real ./internal/meshnet/...
package meshnet

import (
	"bufio"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestRealTailcatLoopback(t *testing.T) {
	serverID, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "client.key"))
	if err != nil {
		t.Fatal(err)
	}
	transport := RealTransport{Logf: t.Logf}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ln, addr, err := transport.Listen(ctx, serverID)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	t.Logf("tailcat address: %s", addr)

	accepted := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- err
			return
		}
		defer c.Close()
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			accepted <- err
			return
		}
		if line != "hello real mesh\n" {
			t.Errorf("server received %q", line)
		}
		_, err = io.WriteString(c, "hi back over real tailcat\n")
		accepted <- err
	}()

	conn, err := transport.Dial(ctx, clientID, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "hello real mesh\n"); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("client received: %q", reply)
	if reply != "hi back over real tailcat\n" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

// TestRealTailcatAddressSurvivesRestart proves the whole point of persisting
// an Identity: a daemon's mesh address must not change when it restarts, or
// every previously-shared --join blob would silently stop working.
func TestRealTailcatAddressSurvivesRestart(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "server.key")
	id, err := LoadOrCreateIdentity(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	transport := RealTransport{Logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ln1, addr1, err := transport.Listen(ctx, id)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	ln1.Close()

	// Simulate a daemon restart: reload the identity from disk, don't reuse
	// the in-memory struct.
	reloaded, err := LoadOrCreateIdentity(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ln2, addr2, err := transport.Listen(ctx, reloaded)
	if err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	defer ln2.Close()

	if addr1 != addr2 {
		t.Fatalf("address changed across a restart: %s -> %s", addr1, addr2)
	}
}
