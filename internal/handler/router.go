package handler

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Tushar-programador/trip-management-api/api"
	"github.com/Tushar-programador/trip-management-api/internal/middleware"
)

// RouterOptions configures cross-cutting HTTP behaviour.
type RouterOptions struct {
	RequestTimeout time.Duration
	MaxBodyBytes   int64
	// Draining is set at the start of shutdown. /readyz then fails while the
	// server keeps serving for SHUTDOWN_DRAIN_DELAY, giving load balancers time
	// to stop routing here before the listener closes.
	Draining *atomic.Bool
}

// NewRouter wires middleware and routes.
func NewRouter(svc TripService, log *slog.Logger, opts RouterOptions) http.Handler {
	h := &TripHandler{svc: svc, log: log}
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(log))
	r.Use(middleware.Recover(log, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, log, &apiError{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "An internal error occurred"})
	}))
	r.Use(middleware.MaxBodyBytes(opts.MaxBodyBytes))
	r.Use(middleware.Timeout(opts.RequestTimeout))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, log, &apiError{Status: http.StatusNotFound, Code: CodeRouteNotFound, Message: "Route not found"})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, log, &apiError{Status: http.StatusMethodNotAllowed, Code: CodeMethodNotAllowed, Message: "Method not allowed"})
	})

	// Liveness: the process is up. Never touches the database, so a database
	// outage does not get healthy API containers restarted.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: the API can serve traffic (database reachable, not draining).
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if opts.Draining != nil && opts.Draining.Load() {
			writeError(w, r, log, &apiError{Status: http.StatusServiceUnavailable, Code: CodeServiceUnavailable, Message: "Server is shutting down"})
			return
		}
		if err := svc.Ready(r.Context()); err != nil {
			log.WarnContext(r.Context(), "readiness check failed", "error", err)
			writeError(w, r, log, &apiError{Status: http.StatusServiceUnavailable, Code: CodeServiceUnavailable, Message: "Database unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(api.OpenAPISpec)
	})

	r.Route("/trips", func(r chi.Router) {
		r.Post("/", h.create)
		r.Get("/", h.list)
		r.Get("/{id}", h.get)
		r.Patch("/{id}/status", h.updateStatus)
		r.Delete("/{id}", h.delete)
	})
	return r
}
