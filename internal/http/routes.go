package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Router builds the chi router. Middleware stack:
//   - RequestID for traceable logs
//   - RealIP so rate limiting (future) and logs see the client IP
//   - Recoverer so a handler panic returns 500 instead of killing the process
//   - Timeout cap as a last line of defense
//
// Handlers register directly; no API versioning yet (out of scope for the lab).
func Router(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	// 5s per request is generous — the slowest path (POST /shorten with DNS
	// resolution) should finish well under this.
	r.Use(middleware.Timeout(requestTimeout))

	r.Get("/healthz", h.Health)
	r.Post("/shorten", h.Shorten)
	r.Get("/stats/{code}", h.Stats)
	r.Get("/{code}", h.Redirect)

	return r
}
