// Package domain holds the trip model and the business rules that do not
// depend on HTTP or PostgreSQL: statuses, allowed transitions, input
// validation and domain errors.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// Status is the lifecycle state of a trip.
type Status string

const (
	StatusCreated   Status = "CREATED"
	StatusAssigned  Status = "ASSIGNED"
	StatusInTransit Status = "IN_TRANSIT"
	StatusCompleted Status = "COMPLETED"
	StatusCancelled Status = "CANCELLED"
)

// AllStatuses lists every valid status in lifecycle order.
var AllStatuses = []Status{StatusCreated, StatusAssigned, StatusInTransit, StatusCompleted, StatusCancelled}

// transitions is the complete state machine. Anything not listed is rejected,
// which makes COMPLETED and CANCELLED terminal.
var transitions = map[Status][]Status{
	StatusCreated:   {StatusAssigned, StatusCancelled},
	StatusAssigned:  {StatusInTransit, StatusCancelled},
	StatusInTransit: {StatusCompleted},
}

// ParseStatus returns the Status for s, or false if s is not a known status.
// Matching is exact and case-sensitive.
func ParseStatus(s string) (Status, bool) {
	for _, st := range AllStatuses {
		if string(st) == s {
			return st, true
		}
	}
	return "", false
}

// IsTerminal reports whether no further transitions are allowed from s.
func (s Status) IsTerminal() bool {
	return len(transitions[s]) == 0
}

// CanTransitionTo reports whether moving from s to next is allowed.
func (s Status) CanTransitionTo(next Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// ValidateTransition returns a *TransitionError if from -> to is not allowed.
func ValidateTransition(from, to Status) error {
	if !from.CanTransitionTo(to) {
		return &TransitionError{From: from, To: to}
	}
	return nil
}

// Trip is a persisted trip.
type Trip struct {
	ID          uuid.UUID `json:"id"`
	TripNumber  string    `json:"trip_number"`
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	DriverName  string    `json:"driver_name"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ListFilter selects a page of trips. Status is optional.
type ListFilter struct {
	Status   *Status
	Page     int
	PageSize int
}

// Offset returns the number of rows to skip for the requested page.
func (f ListFilter) Offset() int {
	return (f.Page - 1) * f.PageSize
}

// TripPage is one page of trips plus the total number of matching trips.
type TripPage struct {
	Trips      []Trip
	TotalItems int
}
