package cron

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"gorm.io/gorm"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
	"github.com/thesahibnanda-max/relay/server/package/session"
)

// fakeMongoPool and fakePostgres are in-memory stand-ins for the two DB
// handles cron depends on, so its scheduling/error-joining logic can be unit
// tested with no real Postgres/Mongo involved - matching this module's
// established fake style (see package sharding's own fakes).
type fakeMongoPool struct {
	mu        sync.Mutex
	pingCalls int
	pingErr   error
	block     chan struct{} // if non-nil, Ping waits on this before returning
}

func (f *fakeMongoPool) Database(mongoURL string) (*mongo.Database, error) {
	return nil, errors.New("cron: Database is not used by this package")
}

func (f *fakeMongoPool) Close(ctx context.Context) error { return nil }

func (f *fakeMongoPool) WithTransaction(ctx context.Context, mongoURL string, fn func(sessCtx context.Context) error) error {
	return errors.New("cron: WithTransaction is not used by this package")
}

func (f *fakeMongoPool) Ping(ctx context.Context) error {
	f.mu.Lock()
	f.pingCalls++
	block, err := f.block, f.pingErr
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func (f *fakeMongoPool) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingCalls
}

type fakePostgres struct {
	mu        sync.Mutex
	pingCalls int
	pingErr   error
}

func (f *fakePostgres) DB() *gorm.DB { return nil }

func (f *fakePostgres) Ping(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingCalls++
	return f.pingErr
}

func (f *fakePostgres) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingCalls
}

// fakeSessions is an in-memory stand-in for session.Interface, just enough
// to prove the sweep/cleanup job's own wiring - every RPC-shaped method is an
// unused no-op.
type fakeSessions struct {
	mu             sync.Mutex
	sweptShards    []string
	deletedShards  []string
	deletedCutoffs []time.Time
	sweepErr       error
	deleteErr      error
}

func (f *fakeSessions) Sweep(ctx context.Context, shardURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweptShards = append(f.sweptShards, shardURL)
	return f.sweepErr
}

func (f *fakeSessions) DeleteSessionsOlderThan(ctx context.Context, shardURL string, cutoff time.Time) (session.DeleteReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedShards = append(f.deletedShards, shardURL)
	f.deletedCutoffs = append(f.deletedCutoffs, cutoff)
	return session.DeleteReport{}, f.deleteErr
}

func (f *fakeSessions) calls() (swept, deleted []string, cutoffs []time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sweptShards...), append([]string(nil), f.deletedShards...), append([]time.Time(nil), f.deletedCutoffs...)
}

func (f *fakeSessions) Join(ctx context.Context, req session.JoinRequest) (session.JoinResult, error) {
	return session.JoinResult{}, nil
}
func (f *fakeSessions) Disconnect(ctx context.Context, sessionID, agentID string) error { return nil }
func (f *fakeSessions) Send(ctx context.Context, sessionID, fromAgentID string, req session.SendRequest) (session.SendOutcome, error) {
	return session.SendOutcome{}, nil
}
func (f *fakeSessions) ListAgents(ctx context.Context, sessionID string) ([]mongodb.Agent, error) {
	return nil, nil
}
func (f *fakeSessions) PendingFor(ctx context.Context, sessionID, agentID string) ([]mongodb.Message, error) {
	return nil, nil
}
func (f *fakeSessions) MarkDispatched(ctx context.Context, sessionID, messageID string) error {
	return nil
}
func (f *fakeSessions) Acknowledge(ctx context.Context, sessionID, agentID, messageID string) error {
	return nil
}
func (f *fakeSessions) ReportState(ctx context.Context, sessionID, agentID, messageID, state string) error {
	return nil
}
func (f *fakeSessions) Wait(ctx context.Context, sessionID, agentID, messageID string, timeout time.Duration) (session.WaitOutcome, error) {
	return session.WaitOutcome{}, nil
}
func (f *fakeSessions) Context(ctx context.Context, sessionID, agentID, forAgentName string, limit int) ([]mongodb.Message, error) {
	return nil, nil
}
func (f *fakeSessions) ListHeld(ctx context.Context, sessionID, agentID string) ([]mongodb.Message, error) {
	return nil, nil
}
func (f *fakeSessions) HeldCount(ctx context.Context, sessionID, agentID string) (int, error) {
	return 0, nil
}
func (f *fakeSessions) Approve(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error) {
	return mongodb.Message{}, nil
}
func (f *fakeSessions) Reject(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error) {
	return mongodb.Message{}, nil
}

var (
	_ mongodb.Interface  = (*fakeMongoPool)(nil)
	_ postgres.Interface = (*fakePostgres)(nil)
	_ session.Interface  = (*fakeSessions)(nil)
)

// testConfig sets PingCheckInterval to interval and SweepEvery to a fixed,
// unrelated hour-long default - every test in this file that doesn't care
// about sweep timing specifically still needs it to be a valid, non-zero
// "@every" schedule for New to succeed at all.
func testConfig(interval time.Duration) config.Config {
	return config.Config{PingCheckInterval: interval, SweepEvery: time.Hour}
}

func TestNew_RejectsNilMongoPool(t *testing.T) {
	if _, err := New(testConfig(time.Minute), nil, &fakePostgres{}, &fakeSessions{}); err == nil {
		t.Fatal("expected an error for a nil mongodbPool, got nil")
	}
}

func TestNew_RejectsNilPostgresConnection(t *testing.T) {
	if _, err := New(testConfig(time.Minute), &fakeMongoPool{}, nil, &fakeSessions{}); err == nil {
		t.Fatal("expected an error for a nil postgresConnection, got nil")
	}
}

func TestNew_RejectsNilSessions(t *testing.T) {
	if _, err := New(testConfig(time.Minute), &fakeMongoPool{}, &fakePostgres{}, nil); err == nil {
		t.Fatal("expected an error for a nil sessions, got nil")
	}
}

func TestNew_BuildsAValidScheduleForEveryConfiguredInterval(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, 5*time.Minute + 30*time.Second, time.Hour} {
		t.Run(interval.String(), func(t *testing.T) {
			if _, err := New(testConfig(interval), &fakeMongoPool{}, &fakePostgres{}, &fakeSessions{}); err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}

// TestPingAll_CallsBothAndReturnsNilWhenBothSucceed exercises the scheduled
// job's own body directly (not through the real scheduler), constructing
// impl by hand - valid since this test file is in the same package.
func TestPingAll_CallsBothAndReturnsNilWhenBothSucceed(t *testing.T) {
	mongoPool := &fakeMongoPool{}
	pg := &fakePostgres{}
	i := impl{mongodbPool: mongoPool, postgresConnection: pg}

	if err := i.pingAll(context.Background()); err != nil {
		t.Fatalf("pingAll: %v", err)
	}
	if mongoPool.calls() != 1 {
		t.Errorf("mongo Ping calls = %d, want 1", mongoPool.calls())
	}
	if pg.calls() != 1 {
		t.Errorf("postgres Ping calls = %d, want 1", pg.calls())
	}
}

// TestPingAll_JoinsBothFailuresWhenBothFail proves a Mongo-only outage isn't
// masked by Postgres still being healthy, or vice versa - errors.Join keeps
// both, this just confirms pingAll still calls both instead of short-
// circuiting on the first failure.
func TestPingAll_JoinsBothFailuresWhenBothFail(t *testing.T) {
	mongoErr := errors.New("mongo is down")
	pgErr := errors.New("postgres is down")
	mongoPool := &fakeMongoPool{pingErr: mongoErr}
	pg := &fakePostgres{pingErr: pgErr}
	i := impl{mongodbPool: mongoPool, postgresConnection: pg}

	err := i.pingAll(context.Background())
	if err == nil {
		t.Fatal("expected a joined error, got nil")
	}
	if !errors.Is(err, mongoErr) {
		t.Errorf("expected the joined error to include the mongo failure: %v", err)
	}
	if !errors.Is(err, pgErr) {
		t.Errorf("expected the joined error to include the postgres failure: %v", err)
	}
	if mongoPool.calls() != 1 || pg.calls() != 1 {
		t.Errorf("expected both to be called exactly once even though mongo failed first: mongo=%d postgres=%d", mongoPool.calls(), pg.calls())
	}
}

// TestStartAndStop_ActuallyRunsTheScheduledJob proves the real wiring
// between New/Start and the scheduler: with a very short interval, the job
// must fire at least once before Stop returns.
func TestStartAndStop_ActuallyRunsTheScheduledJob(t *testing.T) {
	mongoPool := &fakeMongoPool{}
	pg := &fakePostgres{}
	svc, err := New(testConfig(50*time.Millisecond), mongoPool, pg, &fakeSessions{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for mongoPool.calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if mongoPool.calls() == 0 || pg.calls() == 0 {
		t.Fatalf("expected the scheduled job to have run at least once before Stop, got mongo=%d postgres=%d", mongoPool.calls(), pg.calls())
	}
}

// TestStop_RespectsCallerContextTimeout proves Stop doesn't hang forever
// waiting on a stuck job - it must give up once the caller's own ctx expires,
// exactly as documented on the method (an Fx OnStop deadline must still be
// honored even if a ping is hung).
func TestStop_RespectsCallerContextTimeout(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // release the stuck goroutine so the test process can exit cleanly

	mongoPool := &fakeMongoPool{block: block}
	pg := &fakePostgres{}
	svc, err := New(testConfig(10*time.Millisecond), mongoPool, pg, &fakeSessions{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the job a moment to actually start (and block on the channel).
	deadline := time.Now().Add(2 * time.Second)
	for mongoPool.calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if mongoPool.calls() == 0 {
		t.Fatal("the scheduled job never started")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = svc.Stop(stopCtx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected Stop to return an error once its context expired, got nil")
	}
	if elapsed > time.Second {
		t.Fatalf("Stop took %v to return - it should have given up once stopCtx expired (~100ms), not waited on the stuck job", elapsed)
	}
}

// TestSweepAll_CallsSweepForEveryShardAndSurvivesAFailure exercises the sweep
// job's own body directly (not through the real scheduler): every configured
// shard is swept, and one shard's failure must not stop the rest.
func TestSweepAll_CallsSweepForEveryShardAndSurvivesAFailure(t *testing.T) {
	sessions := &fakeSessions{sweepErr: errors.New("shard unreachable")}
	i := impl{sessions: sessions, mongoURLs: []string{"mongodb://a", "mongodb://b"}}

	i.sweepAll(context.Background())

	swept, deleted, _ := sessions.calls()
	if len(swept) != 2 {
		t.Fatalf("expected Sweep called for both shards despite the error, got %v", swept)
	}
	if len(deleted) != 0 {
		t.Fatalf("sweepAll must never call DeleteSessionsOlderThan, got %v", deleted)
	}
}

// TestCleanupOldSessions_CallsDeleteForEveryShardWithTheRightCutoff exercises
// the cleanup job's own body directly: every configured shard is cleaned up,
// with a cutoff derived from sessionMaxAge (not any other duration it could
// be confused with), and a shard's failure doesn't stop the rest.
func TestCleanupOldSessions_CallsDeleteForEveryShardWithTheRightCutoff(t *testing.T) {
	sessions := &fakeSessions{deleteErr: errors.New("shard unreachable")}
	maxAge := 4 * 24 * time.Hour
	i := impl{sessions: sessions, mongoURLs: []string{"mongodb://a", "mongodb://b"}, sessionMaxAge: maxAge}

	before := time.Now().UTC()
	i.cleanupOldSessions(context.Background())

	swept, deleted, cutoffs := sessions.calls()
	if len(swept) != 0 {
		t.Fatalf("cleanupOldSessions must never call Sweep, got %v", swept)
	}
	if len(deleted) != 2 {
		t.Fatalf("expected DeleteSessionsOlderThan called for both shards despite the error, got %v", deleted)
	}
	for _, cutoff := range cutoffs {
		wantAround := before.Add(-maxAge)
		if diff := cutoff.Sub(wantAround); diff < -time.Second || diff > time.Second {
			t.Errorf("cutoff = %v, want close to now-sessionMaxAge (%v)", cutoff, wantAround)
		}
	}
}

// TestStartAndStop_RunsSweepAndCleanupOnTheirOwnIndependentSchedules proves
// the real wiring end to end: with SweepEvery and SessionCleanupInterval set
// to two different short durations, both jobs must actually fire through the
// real scheduler before Stop returns - not just one of them, and not merged
// into a single job.
func TestStartAndStop_RunsSweepAndCleanupOnTheirOwnIndependentSchedules(t *testing.T) {
	cfg := config.Config{
		PingCheckInterval:      time.Hour, // irrelevant here; kept out of the way
		SweepEvery:             20 * time.Millisecond,
		SessionCleanupInterval: 60 * time.Millisecond,
		MongoURLs:              []string{"mongodb://only-shard"},
	}
	sessions := &fakeSessions{}
	svc, err := New(cfg, &fakeMongoPool{}, &fakePostgres{}, sessions)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for sweep (the 3x-faster job) to have fired at least twice AND
	// cleanup to have fired at least once - not just "both have fired," which
	// timing jitter could satisfy after only one tick of each and wouldn't
	// actually distinguish two independent schedules from one merged job.
	deadline := time.Now().Add(3 * time.Second)
	for {
		swept, deleted, _ := sessions.calls()
		if len(swept) >= 2 && len(deleted) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for both jobs to fire: swept=%v deleted=%v", swept, deleted)
		}
		time.Sleep(5 * time.Millisecond)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	swept, deleted, _ := sessions.calls()
	// SweepEvery is a third of SessionCleanupInterval, so by the time cleanup
	// has fired at least once, sweep should have fired at least a couple of
	// times - proving they're genuinely on separate schedules, not the same
	// job double-counted.
	if len(swept) < 2 {
		t.Errorf("expected sweep (the faster job) to have fired several times, got %d", len(swept))
	}
	if len(deleted) == 0 {
		t.Error("expected cleanup to have fired at least once")
	}
}
