package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
)

func TestMapError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error // nil means "passed through unchanged, not a domain error"
	}{
		{"no rows", pgx.ErrNoRows, domain.ErrTripNotFound},
		{"duplicate trip number", &pgconn.PgError{Code: "23505", ConstraintName: "trips_trip_number_key"}, domain.ErrTripNumberExists},
		{"other unique violation", &pgconn.PgError{Code: "23505", ConstraintName: "idempotency_keys_pkey"}, nil},
		{"check violation", &pgconn.PgError{Code: "23514"}, nil},
		{"connection failure", &pgconn.PgError{Code: "08006"}, domain.ErrUnavailable},
		{"admin shutdown (restart/failover)", &pgconn.PgError{Code: "57P01"}, domain.ErrUnavailable},
		{"crash shutdown", &pgconn.PgError{Code: "57P02"}, domain.ErrUnavailable},
		{"cannot connect now (starting up)", &pgconn.PgError{Code: "57P03"}, domain.ErrUnavailable},
		{"lock timeout", &pgconn.PgError{Code: "55P03"}, domain.ErrUnavailable},
		{"statement timeout", &pgconn.PgError{Code: "57014"}, domain.ErrUnavailable},
		{"dropped socket", fmt.Errorf("read: %w", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}), domain.ErrUnavailable},
		{"unexpected EOF", fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF), domain.ErrUnavailable},
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), domain.ErrUnavailable},
		{"client canceled", fmt.Errorf("query: %w", context.Canceled), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mapError(c.err)
			if c.want != nil {
				if !errors.Is(got, c.want) {
					t.Errorf("mapError() = %v, want %v", got, c.want)
				}
				return
			}
			for _, domainErr := range []error{domain.ErrTripNotFound, domain.ErrTripNumberExists, domain.ErrUnavailable} {
				if errors.Is(got, domainErr) {
					t.Errorf("mapError() = %v, must not map to %v", got, domainErr)
				}
			}
			if !errors.Is(got, c.err) {
				t.Errorf("original error lost: %v", got)
			}
		})
	}
}
