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

var (
	_ mongodb.Interface  = (*fakeMongoPool)(nil)
	_ postgres.Interface = (*fakePostgres)(nil)
)

func testConfig(interval time.Duration) config.Config {
	return config.Config{PingCheckInterval: interval}
}

func TestNew_RejectsNilMongoPool(t *testing.T) {
	if _, err := New(testConfig(time.Minute), nil, &fakePostgres{}); err == nil {
		t.Fatal("expected an error for a nil mongodbPool, got nil")
	}
}

func TestNew_RejectsNilPostgresConnection(t *testing.T) {
	if _, err := New(testConfig(time.Minute), &fakeMongoPool{}, nil); err == nil {
		t.Fatal("expected an error for a nil postgresConnection, got nil")
	}
}

func TestNew_BuildsAValidScheduleForEveryConfiguredInterval(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, 5*time.Minute + 30*time.Second, time.Hour} {
		t.Run(interval.String(), func(t *testing.T) {
			if _, err := New(testConfig(interval), &fakeMongoPool{}, &fakePostgres{}); err != nil {
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
	svc, err := New(testConfig(50*time.Millisecond), mongoPool, pg)
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
	svc, err := New(testConfig(10*time.Millisecond), mongoPool, pg)
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
