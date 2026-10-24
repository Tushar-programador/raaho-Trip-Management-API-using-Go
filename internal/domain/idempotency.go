package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// MaxIdempotencyKeyLength matches idempotency_keys.idempotency_key VARCHAR(255).
const MaxIdempotencyKeyLength = 255

// IdempotencyScopeCreateTrip namespaces keys used on POST /trips. With
// authentication this would also include the client/tenant identity.
const IdempotencyScopeCreateTrip = "POST /trips"

// IdempotencyKey is a client-supplied key bound to the fingerprint of the
// request it was first used with.
type IdempotencyKey struct {
	Scope       string
	Key         string
	RequestHash string
	TTL         time.Duration
}

// ValidateIdempotencyKey accepts 1-255 printable ASCII characters (no spaces).
func ValidateIdempotencyKey(key string) error {
	if key == "" || len(key) > MaxIdempotencyKeyLength {
		return &ValidationError{Fields: []FieldError{{
			Field:   "Idempotency-Key",
			Message: "must be between 1 and 255 characters",
		}}}
	}
	for i := 0; i < len(key); i++ {
		if key[i] < '!' || key[i] > '~' {
			return &ValidationError{Fields: []FieldError{{
				Field:   "Idempotency-Key",
				Message: "must contain only printable ASCII characters without spaces",
			}}}
		}
	}
	return nil
}

// Fingerprint returns a SHA-256 hex digest of the normalized input. Struct
// field order makes the JSON encoding deterministic, so semantically equal
// requests (including ones differing only in surrounding whitespace or JSON
// key order) produce the same fingerprint.
func (in CreateTripInput) Fingerprint() string {
	b, err := json.Marshal(in)
	if err != nil {
		// A struct of four strings always marshals.
		panic("domain: marshal CreateTripInput: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
