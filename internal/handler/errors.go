package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
	"github.com/Tushar-programador/trip-management-api/internal/middleware"
)

// Stable, machine-readable error codes. Clients should branch on these, not
// on messages.
const (
	CodeValidation              = "VALIDATION_ERROR"
	CodeInvalidJSON             = "INVALID_JSON"
	CodeUnsupportedMediaType    = "UNSUPPORTED_MEDIA_TYPE"
	CodePayloadTooLarge         = "PAYLOAD_TOO_LARGE"
	CodeTripNotFound            = "TRIP_NOT_FOUND"
	CodeTripNumberExists        = "TRIP_NUMBER_EXISTS"
	CodeInvalidStatus           = "INVALID_STATUS"
	CodeInvalidStatusTransition = "INVALID_STATUS_TRANSITION"
	CodeIdempotencyKeyReused    = "IDEMPOTENCY_KEY_REUSED"
	CodeRouteNotFound           = "ROUTE_NOT_FOUND"
	CodeMethodNotAllowed        = "METHOD_NOT_ALLOWED"
	CodeInternal                = "INTERNAL_ERROR"
	CodeServiceUnavailable      = "SERVICE_UNAVAILABLE"
)

// StatusClientClosedRequest is recorded (in logs only) when the client
// disconnects before the response is ready. It is a de facto standard from nginx.
const StatusClientClosedRequest = 499

// apiError is an error raised by the HTTP layer itself (bad JSON, wrong
// content type, ...) that already knows its status and code.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Message }

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string              `json:"code"`
	Message   string              `json:"message"`
	RequestID string              `json:"request_id,omitempty"`
	Details   []domain.FieldError `json:"details,omitempty"`
}

// mapError converts any error into the HTTP status and client-safe error
// payload. Unknown errors become a generic 500 so internal details (SQL,
// driver messages) never reach the client.
func mapError(err error) (int, errorDetail) {
	var (
		apiErr        *apiError
		validationErr *domain.ValidationError
		statusErr     *domain.InvalidStatusError
		transitionErr *domain.TransitionError
	)
	switch {
	case errors.As(err, &apiErr):
		return apiErr.Status, errorDetail{Code: apiErr.Code, Message: apiErr.Message}
	case errors.As(err, &validationErr):
		return http.StatusBadRequest, errorDetail{Code: CodeValidation, Message: "Request validation failed", Details: validationErr.Fields}
	case errors.As(err, &statusErr):
		return http.StatusBadRequest, errorDetail{Code: CodeInvalidStatus, Message: statusErr.Error()}
	case errors.As(err, &transitionErr):
		return http.StatusConflict, errorDetail{Code: CodeInvalidStatusTransition, Message: transitionErr.Error()}
	case errors.Is(err, domain.ErrTripNotFound):
		return http.StatusNotFound, errorDetail{Code: CodeTripNotFound, Message: "Trip not found"}
	case errors.Is(err, domain.ErrTripNumberExists):
		return http.StatusConflict, errorDetail{Code: CodeTripNumberExists, Message: "A trip with this trip_number already exists"}
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		return http.StatusConflict, errorDetail{Code: CodeIdempotencyKeyReused, Message: "Idempotency-Key has already been used with a different request body"}
	case errors.Is(err, domain.ErrUnavailable):
		return http.StatusServiceUnavailable, errorDetail{Code: CodeServiceUnavailable, Message: "Service temporarily unavailable, please retry"}
	default:
		return http.StatusInternalServerError, errorDetail{Code: CodeInternal, Message: "An internal error occurred"}
	}
}

// writeError logs server-side failures with full detail and sends the client
// the sanitized payload plus the request ID for correlation.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		// The client disconnected, so nobody will read a response and nothing
		// failed on our side. Record it with nginx's 499 rather than as a 5xx.
		log.InfoContext(r.Context(), "client closed request", "request_id", middleware.RequestIDFromContext(r.Context()))
		w.WriteHeader(StatusClientClosedRequest)
		return
	}

	status, detail := mapError(err)
	detail.RequestID = middleware.RequestIDFromContext(r.Context())
	if status >= 500 {
		log.ErrorContext(r.Context(), "request failed", "request_id", detail.RequestID, "status", status, "error", err)
	}
	writeJSON(w, status, errorBody{Error: detail})
}
