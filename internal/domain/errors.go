package domain

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrTripNotFound means no trip exists with the requested ID.
	ErrTripNotFound = errors.New("trip not found")
	// ErrTripNumberExists means the UNIQUE constraint on trip_number rejected the insert.
	ErrTripNumberExists = errors.New("trip number already exists")
	// ErrIdempotencyKeyReused means an Idempotency-Key was replayed with a different request body.
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different request")
	// ErrUnavailable means a dependency (the database) cannot serve the request right now.
	ErrUnavailable = errors.New("service temporarily unavailable")
	// ErrUnauthenticated means the request has no API key, or one that is
	// malformed, unknown or revoked. The cases are deliberately not distinguished.
	ErrUnauthenticated = errors.New("missing or invalid API key")
	// ErrClientNotFound means no API client exists with the requested ID.
	ErrClientNotFound = errors.New("API client not found")
)

// TransitionError is returned when a status change violates the state machine.
type TransitionError struct {
	From Status
	To   Status
}

func (e *TransitionError) Error() string {
	if e.From.IsTerminal() {
		return fmt.Sprintf("trip is in terminal status %s and cannot transition to %s", e.From, e.To)
	}
	return fmt.Sprintf("cannot transition trip from %s to %s", e.From, e.To)
}

// FieldError describes one invalid input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError collects every invalid field so clients can fix them all at once.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + " " + f.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// InvalidStatusError is returned when a status string is not one of the five known values.
type InvalidStatusError struct {
	Value string
}

func (e *InvalidStatusError) Error() string {
	return fmt.Sprintf("invalid status %q: must be one of %s", e.Value, statusList())
}

func statusList() string {
	names := make([]string, len(AllStatuses))
	for i, s := range AllStatuses {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}
