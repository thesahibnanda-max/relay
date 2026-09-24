package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/session"
)

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

func (fakeSessionService) Approve(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error) {
	return mongodb.Message{}, nil
}

func (fakeSessionService) Reject(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error) {
	return mongodb.Message{}, nil
}

func (fakeSessionService) Sweep(ctx context.Context, shardURL string) error {
	return nil
}

var _ session.Interface = fakeSessionService{}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	hub, err := New(fakeSessionService{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return NewHandler(hub)
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
