// Package auth implements API-key authentication: generating and hashing
// keys, and the HTTP middleware that resolves a bearer key to a client.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
	"github.com/Tushar-programador/trip-management-api/internal/middleware"
)

const (
	// keyScheme makes keys recognisable, e.g. by secret scanners.
	keyScheme      = "tma_"
	keyRandomBytes = 32 // 256 bits of entropy
	// KeyLength is the length of every key: the scheme plus 32 bytes in
	// unpadded base64url.
	KeyLength = len(keyScheme) + 43
	// displayPrefixLength characters identify a key in listings and logs
	// while leaving more than 200 bits secret.
	displayPrefixLength = 12
)

// NewKey returns a new random API key.
func NewKey() (string, error) {
	b := make([]byte, keyRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate key: %w", err)
	}
	return keyScheme + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashKey returns the hex SHA-256 of key, the only form in which keys are
// stored. A fast hash is appropriate here: unlike passwords, keys have 256
// bits of entropy, so a leaked hash cannot be brute-forced back to a key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// DisplayPrefix returns the leading characters of a well-formed key.
func DisplayPrefix(key string) string {
	return key[:displayPrefixLength]
}

// WellFormed reports whether s has the shape of a key, so obvious garbage is
// rejected without a database lookup.
func WellFormed(s string) bool {
	if len(s) != KeyLength || !strings.HasPrefix(s, keyScheme) {
		return false
	}
	for i := len(keyScheme); i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ClientStore resolves a key hash to an active (not revoked) client. It
// returns domain.ErrUnauthenticated when there is none.
type ClientStore interface {
	FindActiveByKeyHash(ctx context.Context, keyHash string) (domain.Client, error)
}

// Middleware requires "Authorization: Bearer <key>". A missing, malformed,
// unknown or revoked key is reported to fail as domain.ErrUnauthenticated;
// if the lookup itself fails (database unavailable) that error is passed on
// instead, so it can be answered with a 503 rather than a misleading 401.
func Middleware(store ClientStore, fail func(http.ResponseWriter, *http.Request, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok || !WellFormed(key) {
				fail(w, r, domain.ErrUnauthenticated)
				return
			}
			client, err := store.FindActiveByKeyHash(r.Context(), HashKey(key))
			if err != nil {
				fail(w, r, err)
				return
			}
			middleware.SetClientID(r.Context(), client.ID.String())
			next.ServeHTTP(w, r.WithContext(WithClient(r.Context(), client)))
		})
	}
}

// bearerToken extracts the credential from an RFC 6750 Authorization header.
// The scheme name is case-insensitive.
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

type ctxKey struct{}

// WithClient returns a context carrying the authenticated client.
func WithClient(ctx context.Context, c domain.Client) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// ClientFromContext returns the client set by Middleware.
func ClientFromContext(ctx context.Context) (domain.Client, bool) {
	c, ok := ctx.Value(ctxKey{}).(domain.Client)
	return c, ok
}
