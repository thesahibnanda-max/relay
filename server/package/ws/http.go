package ws

import "net/http"

// Path is the WebSocket upgrade endpoint agents connect to.
const Path = "/v1/agent"

// NewHandler builds the HTTP handler that serves h's WebSocket endpoint plus
// a bare health check, wrapped in a CORS middleware that allows every
// origin and every header - the WebSocket upgrade itself is separately
// allowed via AcceptOptions.InsecureSkipVerify in Accept (see hub.go);
// this middleware is what covers ordinary HTTP requests (health checks,
// preflight OPTIONS) hitting this same server.
func NewHandler(h Interface) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Path, h.Accept)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
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
