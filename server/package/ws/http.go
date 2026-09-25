package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
)

// Path is the WebSocket upgrade endpoint agents connect to.
const Path = "/v1/agent"

// healthzTimeout bounds each on-demand Ping /healthz makes, so a genuinely
// unreachable database reports "unhealthy" quickly instead of the caller
// hanging on a long default network timeout.
const healthzTimeout = 5 * time.Second

// healthzResponse is the JSON body /healthz replies with. SQL/NoSQL are
// either "healthy" or "unhealthy: <the ping's own error text>" - the raw
// error is included on purpose, so a human hitting this during an incident
// sees the actual failure, not just "something's wrong."
type healthzResponse struct {
	Status string `json:"status"`
	SQL    string `json:"sql"`
	NoSQL  string `json:"no-sql"`
}

// NewHandler builds the HTTP handler that serves h's WebSocket endpoint plus
// a health check that actually pings sqlDB/noSQLDB (not just "the process is
// up"), wrapped in a CORS middleware that allows every origin and every
// header - the WebSocket upgrade itself is separately allowed via
// AcceptOptions.InsecureSkipVerify in Accept (see hub.go); this middleware
// is what covers ordinary HTTP requests (health checks, preflight OPTIONS)
// hitting this same server.
func NewHandler(h Interface, sqlDB postgres.Interface, noSQLDB mongodb.Interface) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Path, h.Accept)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthzTimeout)
		defer cancel()

		resp := healthzResponse{Status: "healthy", SQL: "healthy", NoSQL: "healthy"}
		if err := sqlDB.Ping(ctx); err != nil {
			resp.Status, resp.SQL = "unhealthy", "unhealthy: "+err.Error()
		}
		if err := noSQLDB.Ping(ctx); err != nil {
			resp.Status, resp.NoSQL = "unhealthy", "unhealthy: "+err.Error()
		}

		statusCode := http.StatusOK
		if resp.Status != "healthy" {
			statusCode = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_ = json.NewEncoder(w).Encode(resp)
	})
	return corsMiddleware(mux)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
			w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
		} else {
			w.Header().Set("Access-Control-Allow-Headers", "*")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
