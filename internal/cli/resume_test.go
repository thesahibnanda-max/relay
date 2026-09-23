package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/daemon"
	"github.com/thesahibnanda-max/relay/internal/link"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// newResumeTestDaemon starts a real in-process daemon on its own home, for
// exercising connectWithResume directly (faster and more precise than the
// full binary-spawning e2e tests for the fallback/retry logic specifically).
func newResumeTestDaemon(t *testing.T) relayhome.Paths {
	t.Helper()
	root, err := os.MkdirTemp("", "rlresume")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	paths := relayhome.Paths{Root: root}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	srv, err := daemon.New(daemon.Options{Paths: paths, Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", paths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return paths
}

func baseOpt(paths relayhome.Paths, session, name string) link.Options {
	return link.Options{Paths: paths, Hello: proto.Hello{Session: session, Name: name, Tool: "claude", Role: "developer"}}
}

func TestConnectWithResumeSucceedsWithASavedToken(t *testing.T) {
	paths := newResumeTestDaemon(t)
	ctx := context.Background()

	first, err := link.Connect(ctx, baseOpt(paths, proto.SessionNew, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	id := first.Identity()
	first.Close(0)

	saveIdentity(paths, id.Session.ID, "alice", id.Token, "claude")
	p := Parsed{Session: id.Session.ID, Name: "alice"}
	lk, err := connectWithResume(ctx, paths, p, baseOpt(paths, p.Session, p.Name))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close(0)
	if !lk.Identity().Resumed || lk.Identity().Agent.ID != id.Agent.ID {
		t.Fatalf("expected a resume of the same agent, got %+v", lk.Identity())
	}
}

func TestConnectWithResumeFallsBackOnAStaleToken(t *testing.T) {
	paths := newResumeTestDaemon(t)
	ctx := context.Background()

	// A saved identity naming a session that doesn't exist at all - as
	// stale as it gets.
	saveIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "bob", "wrongtoken", "claude")

	p := Parsed{Session: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "bob"}
	_, err := connectWithResume(ctx, paths, p, baseOpt(paths, p.Session, p.Name))
	// The fallback fresh registration also fails, because the session truly
	// doesn't exist - but it must fail as CodeSessionNotFound (from the
	// *fallback* registration attempt), not surface the original resume
	// error, and it must have already deleted the stale file either way.
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeSessionNotFound {
		t.Fatalf("expected the fallback registration's own error, got %v", err)
	}
	if _, ok := loadIdentity(paths, p.Session, p.Name); ok {
		t.Fatal("the stale identity file should have been deleted")
	}
}

func TestConnectWithResumeFlagFailsLoudlyInsteadOfFallingBack(t *testing.T) {
	paths := newResumeTestDaemon(t)
	ctx := context.Background()
	saveIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "bob", "wrongtoken", "claude")

	p := Parsed{Session: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "bob", Resume: true}
	if _, err := connectWithResume(ctx, paths, p, baseOpt(paths, p.Session, p.Name)); err == nil {
		t.Fatal("expected an error, not a silent fallback")
	}
	if _, ok := loadIdentity(paths, p.Session, p.Name); ok {
		t.Fatal("the stale identity should still be deleted even when --resume fails loudly")
	}
}

func TestConnectWithResumeFlagFailsLoudlyWithNoSavedIdentity(t *testing.T) {
	paths := newResumeTestDaemon(t)
	ctx := context.Background()

	first, err := link.Connect(ctx, baseOpt(paths, proto.SessionNew, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	id := first.Identity()
	first.Close(0)

	p := Parsed{Session: id.Session.ID, Name: "someone-else", Resume: true}
	if _, err := connectWithResume(ctx, paths, p, baseOpt(paths, p.Session, p.Name)); err == nil {
		t.Fatal("expected an error: there is nothing saved to resume")
	}
}

// TestConnectWithResumeFreshSkipsTheSavedIdentity proves --fresh never even
// attempts the saved token: a plain registration under the same name is
// attempted instead, which fails with CodeNameTaken because the old
// (exited, but not yet GC'd) agent still occupies that name - a resume
// attempt would have succeeded instead, using the very same valid token.
func TestConnectWithResumeFreshSkipsTheSavedIdentity(t *testing.T) {
	paths := newResumeTestDaemon(t)
	ctx := context.Background()

	first, err := link.Connect(ctx, baseOpt(paths, proto.SessionNew, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	id := first.Identity()
	first.Close(0)
	saveIdentity(paths, id.Session.ID, "alice", id.Token, "claude")

	p := Parsed{Session: id.Session.ID, Name: "alice", Fresh: true}
	_, err = connectWithResume(ctx, paths, p, baseOpt(paths, p.Session, p.Name))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeNameTaken {
		t.Fatalf("expected --fresh to attempt (and fail) a plain registration, got %v", err)
	}
}

func TestConnectWithResumeSkipsLookupWithoutAName(t *testing.T) {
	paths := newResumeTestDaemon(t)
	ctx := context.Background()

	seed, err := link.Connect(ctx, baseOpt(paths, proto.SessionNew, "seed"))
	if err != nil {
		t.Fatal(err)
	}
	sessionID := seed.Identity().Session.ID
	seed.Close(0)
	saveIdentity(paths, sessionID, "alice", "sekret", "claude") // irrelevant: no name is given below

	p := Parsed{Session: sessionID}
	lk, err := connectWithResume(ctx, paths, p, baseOpt(paths, sessionID, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close(0)
	if lk.Identity().Resumed {
		t.Fatal("must not resume when no name was given")
	}
}
