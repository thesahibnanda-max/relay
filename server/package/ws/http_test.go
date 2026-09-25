package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"gorm.io/gorm"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
	"github.com/thesahibnanda-max/relay/server/package/session"
)

// testConfig mirrors the defaults config.New would produce, without needing
// real env vars set for a pure HTTP/CORS test.
func testConfig() config.Config {
	return config.Config{
		PairRateLimit: 20, SenderRateLimit: 60, RPCRateLimit: 200,
		RPCRateWindow: 10 * time.Second, MaxInFlightRPCs: 16,
	}
}

// fakeSessionService is a no-op stand-in for session.Interface, just enough
// to satisfy ws.New for a pure HTTP/CORS test - no real database involved.
type fakeSessionService struct{}

func (fakeSessionService) Join(ctx context.Context, req session.JoinRequest) (session.JoinResult, error) {
	return session.JoinResult{}, nil
}

func (fakeSessionService) Disconnect(ctx context.Context, sessionID, agentID string) error {
	return nil
}

func (fakeSessionService) Send(ctx context.Context, sessionID, fromAgentID string, req session.SendRequest) (session.SendOutcome, error) {
	return session.SendOutcome{}, nil
}

func (fakeSessionService) ListAgents(ctx context.Context, sessionID string) ([]mongodb.Agent, error) {
	return nil, nil
}

func (fakeSessionService) PendingFor(ctx context.Context, sessionID, agentID string) ([]mongodb.Message, error) {
	return nil, nil
}

func (fakeSessionService) MarkDispatched(ctx context.Context, sessionID, messageID string) error {
	return nil
}

func (fakeSessionService) Acknowledge(ctx context.Context, sessionID, agentID, messageID string) error {
	return nil
}

func (fakeSessionService) Wait(ctx context.Context, sessionID, agentID, messageID string, timeout time.Duration) (session.WaitOutcome, error) {
	return session.WaitOutcome{}, nil
}

func (fakeSessionService) Context(ctx context.Context, sessionID, agentID, forAgentName string, limit int) ([]mongodb.Message, error) {
	return nil, nil
}

func (fakeSessionService) ReportState(ctx context.Context, sessionID, agentID, messageID, state string) error {
	return nil
}

func (fakeSessionService) ListHeld(ctx context.Context, sessionID, agentID string) ([]mongodb.Message, error) {
	return nil, nil
}

func (fakeSessionService) HeldCount(ctx context.Context, sessionID, agentID string) (int, error) {
	return 0, nil
}

func (fakeSessionService) Approve(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error) {
	return mongodb.Message{}, nil
}

func (fakeSessionService) Reject(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error) {
	return mongodb.Message{}, nil
}

func (fakeSessionService) Sweep(ctx context.Context, shardURL string) error {
	return nil
}

func (fakeSessionService) DeleteSessionsOlderThan(ctx context.Context, shardURL string, cutoff time.Time) (session.DeleteReport, error) {
	return session.DeleteReport{}, nil
}

var _ session.Interface = fakeSessionService{}

// fakePostgresPinger and fakeMongoPinger are controllable stand-ins for
// postgres.Interface/mongodb.Interface, just enough to drive /healthz's
// two independent outcomes without a real database.
type fakePostgresPinger struct{ err error }

func (f fakePostgresPinger) DB() *gorm.DB               { return nil }
func (f fakePostgresPinger) Ping(context.Context) error { return f.err }

type fakeMongoPinger struct{ err error }

func (f fakeMongoPinger) Database(mongoURL string) (*mongo.Database, error) {
	return nil, errors.New("fakeMongoPinger: Database is not used by these tests")
}
func (f fakeMongoPinger) Close(context.Context) error { return nil }
func (f fakeMongoPinger) Ping(context.Context) error  { return f.err }
func (f fakeMongoPinger) WithTransaction(ctx context.Context, mongoURL string, fn func(sessCtx context.Context) error) error {
	return errors.New("fakeMongoPinger: WithTransaction is not used by these tests")
}

var (
	_ postgres.Interface = fakePostgresPinger{}
	_ mongodb.Interface  = fakeMongoPinger{}
)

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return newTestHandlerWithPings(t, nil, nil)
}

func newTestHandlerWithPings(t *testing.T, sqlErr, noSQLErr error) http.Handler {
	t.Helper()
	hub, err := New(testConfig(), fakeSessionService{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return NewHandler(hub, fakePostgresPinger{err: sqlErr}, fakeMongoPinger{err: noSQLErr})
}

// TestCORS_AllowsAnyOriginOnAPlainRequest is the plan's "manual CORS check,"
// made automatic: any Origin must be echoed back as allowed on an ordinary
// HTTP request, since the requirement is "all origins, all headers."
func TestCORS_AllowsAnyOriginOnAPlainRequest(t *testing.T) {
	handler := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz: got status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin: got %q, want %q", got, "*")
	}
}

// TestCORS_PreflightReflectsRequestedHeaders checks the OPTIONS preflight
// path: it must succeed with 204 and allow whatever headers the browser
// asked to send, satisfying "all headers in req/res."
func TestCORS_PreflightReflectsRequestedHeaders(t *testing.T) {
	handler := newTestHandler(t)

	req := httptest.NewRequest(http.MethodOptions, "/healthz", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "X-Custom-Header, Authorization")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS preflight: got status %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin: got %q, want %q", got, "*")
	}
	if got, want := rec.Header().Get("Access-Control-Allow-Headers"), "X-Custom-Header, Authorization"; got != want {
		t.Errorf("Access-Control-Allow-Headers: got %q, want %q", got, want)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "*" {
		t.Errorf("Access-Control-Allow-Methods: got %q, want %q", got, "*")
	}
}

// TestCORS_PreflightWithNoRequestedHeadersFallsBackToStar covers a preflight
// that names no specific headers - it must still allow everything, not
// nothing.
func TestCORS_PreflightWithNoRequestedHeadersFallsBackToStar(t *testing.T) {
	handler := newTestHandler(t)

	req := httptest.NewRequest(http.MethodOptions, "/healthz", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "*" {
		t.Errorf("Access-Control-Allow-Headers: got %q, want %q", got, "*")
	}
}

type healthzBody struct {
	Status string `json:"status"`
	SQL    string `json:"sql"`
	NoSQL  string `json:"no-sql"`
}

func getHealthz(t *testing.T, handler http.Handler) (int, healthzBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q, want %q", got, "application/json")
	}
	var body healthzBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding /healthz body: %v", err)
	}
	return rec.Code, body
}

// TestHealthz_ReturnsJSONStatus proves /healthz is a real JSON status
// endpoint, not just a bare 200 - useful for anything (a deploy script, a
// monitoring check) that wants to distinguish "server answered" from
// "server answered and says it's healthy," and that it actually pings both
// databases (all fields "healthy" here, since both fakes succeed).
func TestHealthz_ReturnsJSONStatus(t *testing.T) {
	handler := newTestHandler(t)

	code, body := getHealthz(t, handler)
	if code != http.StatusOK {
		t.Fatalf("GET /healthz: got status %d, want 200", code)
	}
	if body != (healthzBody{Status: "healthy", SQL: "healthy", NoSQL: "healthy"}) {
		t.Errorf("unexpected body: %+v", body)
	}
}

// TestHealthz_ReportsSQLFailureWithoutMaskingIt proves a failing Postgres
// ping surfaces as an overall-unhealthy 503 with the exact error text in
// "sql", not just a generic "something's wrong" - and that a Mongo failure
// (below) doesn't get confused with a SQL one, or vice versa.
func TestHealthz_ReportsSQLFailureWithoutMaskingIt(t *testing.T) {
	handler := newTestHandlerWithPings(t, errors.New("connection refused"), nil)

	code, body := getHealthz(t, handler)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz: got status %d, want 503", code)
	}
	if body.Status != "unhealthy" {
		t.Errorf(`Status = %q, want "unhealthy"`, body.Status)
	}
	if want := "unhealthy: connection refused"; body.SQL != want {
		t.Errorf("SQL = %q, want %q", body.SQL, want)
	}
	if body.NoSQL != "healthy" {
		t.Errorf(`NoSQL = %q, want "healthy" (Mongo never failed)`, body.NoSQL)
	}
}

// TestHealthz_ReportsMongoFailureWithoutMaskingIt is
// TestHealthz_ReportsSQLFailureWithoutMaskingIt's mirror image for the
// MongoDB side.
func TestHealthz_ReportsMongoFailureWithoutMaskingIt(t *testing.T) {
	handler := newTestHandlerWithPings(t, nil, errors.New("server selection timeout"))

	code, body := getHealthz(t, handler)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz: got status %d, want 503", code)
	}
	if body.Status != "unhealthy" {
		t.Errorf(`Status = %q, want "unhealthy"`, body.Status)
	}
	if body.SQL != "healthy" {
		t.Errorf(`SQL = %q, want "healthy" (Postgres never failed)`, body.SQL)
	}
	if want := "unhealthy: server selection timeout"; body.NoSQL != want {
		t.Errorf("NoSQL = %q, want %q", body.NoSQL, want)
	}
}

// TestHealthz_ReportsBothFailuresIndependently proves the two checks are
// genuinely independent - both bad at once still reports each one's own
// distinct error, not just the first one found.
func TestHealthz_ReportsBothFailuresIndependently(t *testing.T) {
	handler := newTestHandlerWithPings(t, errors.New("sql is down"), errors.New("mongo is down"))

	code, body := getHealthz(t, handler)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz: got status %d, want 503", code)
	}
	if want := "unhealthy: sql is down"; body.SQL != want {
		t.Errorf("SQL = %q, want %q", body.SQL, want)
	}
	if want := "unhealthy: mongo is down"; body.NoSQL != want {
		t.Errorf("NoSQL = %q, want %q", body.NoSQL, want)
	}
}
