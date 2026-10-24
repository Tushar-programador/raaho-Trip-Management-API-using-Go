package domain

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MaxClientNameLength matches api_clients.name VARCHAR(100).
const MaxClientNameLength = 100

// Client is an API consumer that authenticates with an API key. The key
// itself is never stored or held here; KeyPrefix identifies it for humans.
type Client struct {
	ID        uuid.UUID
	Name      string
	KeyPrefix string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// ValidateClientName checks a (trimmed) client name.
func ValidateClientName(name string) error {
	var msg string
	switch {
	case name == "":
		msg = "is required"
	case utf8.RuneCountInString(name) > MaxClientNameLength:
		msg = "must be at most 100 characters"
	case strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }):
		msg = "must not contain control characters"
	default:
		return nil
	}
	return &ValidationError{Fields: []FieldError{{Field: "name", Message: msg}}}
}
