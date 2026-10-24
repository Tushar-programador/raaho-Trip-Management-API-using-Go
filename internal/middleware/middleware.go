// Package middleware provides the HTTP cross-cutting concerns: request IDs,
// panic recovery, structured access logging, request timeouts and body limits.
package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// RequestIDHeader carries the request ID in both directions.
const RequestIDHeader = "X-Request-ID"

type ctxKey struct{}

// RequestIDFromContext returns the request ID set by RequestID, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// RequestID reuses a well-formed inbound X-Request-ID (so IDs can be
// correlated across services) or generates a new one, stores it in the
// request context and echoes it in the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// validRequestID accepts 1-128 characters from a conservative set, so a
// client cannot inject arbitrary content into logs or response headers.
func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// Recover turns a panic into a logged 500 response produced by onPanic. The
// stack trace goes to the log only, never to the client.
func Recover(log *slog.Logger, onPanic http.HandlerFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					panic(rec) // deliberate abort; let net/http handle it
				}
				log.ErrorContext(r.Context(), "panic recovered",
					"request_id", RequestIDFromContext(r.Context()),
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				onPanic(w, r)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestMeta collects facts discovered further down the chain (such as the
// authenticated client) for the access log line written by Logger.
type requestMeta struct {
	clientID string
}

type metaKey struct{}

// SetClientID records the authenticated client for the request's access log
// line. It is a no-op outside Logger.
func SetClientID(ctx context.Context, id string) {
	if m, ok := ctx.Value(metaKey{}).(*requestMeta); ok {
		m.clientID = id
	}
}

// Logger writes one structured log line per request. Bodies, headers and
// query strings are not logged, so no request data or secrets leak into logs.
// Health-check probes are logged at debug level to keep the log readable.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			meta := &requestMeta{}
			r = r.WithContext(context.WithValue(r.Context(), metaKey{}, meta))
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelInfo
			switch {
			case status >= 500:
				level = slog.LevelError
			case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
				level = slog.LevelDebug
			}
			route := ""
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				route = rctx.RoutePattern()
			}
			attrs := []any{
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"route", route,
				"status", status,
				"bytes", ww.BytesWritten(),
				"duration_ms", float64(time.Since(start).Microseconds()) / 1000,
			}
			if meta.clientID != "" {
				attrs = append(attrs, "client_id", meta.clientID)
			}
			log.Log(r.Context(), level, "http request", attrs...)
		})
	}
}

// Timeout bounds the request context, which is propagated to every database
// call, so a slow query cannot hold a connection past the deadline.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// MaxBodyBytes caps the request body. Reads past the limit fail with
// *http.MaxBytesError, which the JSON decoder reports as 413.
func MaxBodyBytes(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}
