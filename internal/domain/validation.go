package domain

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Maximum field lengths in characters. They match the VARCHAR sizes in
// migrations/000001_create_trips.up.sql.
const (
	MaxTripNumberLength  = 64
	MaxSourceLength      = 255
	MaxDestinationLength = 255
	MaxDriverNameLength  = 100
)

// CreateTripInput is the client-supplied part of a new trip.
type CreateTripInput struct {
	TripNumber  string `json:"trip_number"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	DriverName  string `json:"driver_name"`
}

// Normalize trims leading and trailing whitespace from every field. The
// normalized values are what get validated, stored and fingerprinted.
func (in CreateTripInput) Normalize() CreateTripInput {
	return CreateTripInput{
		TripNumber:  strings.TrimSpace(in.TripNumber),
		Source:      strings.TrimSpace(in.Source),
		Destination: strings.TrimSpace(in.Destination),
		DriverName:  strings.TrimSpace(in.DriverName),
	}
}

// Validate checks a normalized input and reports every invalid field.
func (in CreateTripInput) Validate() error {
	var errs []FieldError
	check := func(field, value string, maxLen int) {
		switch {
		case value == "":
			errs = append(errs, FieldError{Field: field, Message: "is required"})
		case utf8.RuneCountInString(value) > maxLen:
			errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf("must be at most %d characters", maxLen)})
		case containsControl(value):
			errs = append(errs, FieldError{Field: field, Message: "must not contain control characters"})
		}
	}
	check("trip_number", in.TripNumber, MaxTripNumberLength)
	check("source", in.Source, MaxSourceLength)
	check("destination", in.Destination, MaxDestinationLength)
	check("driver_name", in.DriverName, MaxDriverNameLength)

	if len(errs) > 0 {
		return &ValidationError{Fields: errs}
	}
	return nil
}

// containsControl rejects NUL bytes (which PostgreSQL text cannot store) and
// other control characters such as newlines that have no place in these fields.
func containsControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}
