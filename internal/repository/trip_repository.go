// Package repository implements trip persistence on PostgreSQL. It owns all
// SQL and every transaction boundary; callers see only domain types and
// domain errors.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
)

const tripColumns = `id, trip_number, source, destination, driver_name, status, created_at, updated_at`

// TripRepository persists trips in PostgreSQL.
type TripRepository struct {
	pool *pgxpool.Pool
}

// NewTripRepository returns a repository backed by pool.
func NewTripRepository(pool *pgxpool.Pool) *TripRepository {
	return &TripRepository{pool: pool}
}

// Create inserts trip and returns the persisted row. When key is non-nil the
// insert is idempotent: see createIdempotent.
func (r *TripRepository) Create(ctx context.Context, trip domain.Trip, key *domain.IdempotencyKey) (created domain.Trip, replayed bool, err error) {
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if key != nil {
			prior, found, err := claimIdempotencyKey(ctx, tx, key)
			if err != nil {
				return err
			}
			if found {
				created, replayed = prior, true
				return nil
			}
		}

		created, err = insertTrip(ctx, tx, trip)
		if err != nil {
			return err
		}

		if key != nil {
			return completeIdempotencyKey(ctx, tx, key, created)
		}
		return nil
	})
	if err != nil {
		return domain.Trip{}, false, mapError(err)
	}
	return created, replayed, nil
}

// claimIdempotencyKey inserts the key row inside the caller's transaction.
//
//   - New key: the row is inserted and this transaction owns the key.
//   - Expired key: the ON CONFLICT ... WHERE clause resets it and this
//     transaction owns it again.
//   - Live key held by an in-flight transaction: PostgreSQL blocks this
//     INSERT on the unique index until that transaction ends. If it rolled
//     back, our insert proceeds; if it committed, the conflict path runs.
//   - Live, committed key: zero rows are affected and the stored result is
//     read back. Under READ COMMITTED the follow-up SELECT is a new statement
//     and therefore sees the row the blocking transaction just committed.
func claimIdempotencyKey(ctx context.Context, tx pgx.Tx, key *domain.IdempotencyKey) (domain.Trip, bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (scope, idempotency_key, request_hash, created_at, expires_at)
		VALUES ($1, $2, $3, now(), now() + make_interval(secs => $4))
		ON CONFLICT (scope, idempotency_key) DO UPDATE
		   SET request_hash  = EXCLUDED.request_hash,
		       trip_id       = NULL,
		       trip_snapshot = NULL,
		       created_at    = EXCLUDED.created_at,
		       expires_at    = EXCLUDED.expires_at
		 WHERE idempotency_keys.expires_at <= now()`,
		key.Scope, key.Key, key.RequestHash, key.TTL.Seconds())
	if err != nil {
		return domain.Trip{}, false, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return domain.Trip{}, false, nil
	}

	var storedHash string
	var snapshot []byte
	err = tx.QueryRow(ctx, `
		SELECT request_hash, trip_snapshot
		  FROM idempotency_keys
		 WHERE scope = $1 AND idempotency_key = $2`,
		key.Scope, key.Key).Scan(&storedHash, &snapshot)
	if err != nil {
		return domain.Trip{}, false, fmt.Errorf("read idempotency key: %w", err)
	}
	if storedHash != key.RequestHash {
		return domain.Trip{}, false, domain.ErrIdempotencyKeyReused
	}
	if snapshot == nil {
		// Unreachable: a key row is inserted and completed in the same
		// transaction, so other sessions never see it incomplete. Fail loudly
		// rather than replay an empty result.
		return domain.Trip{}, false, errors.New("idempotency key has no stored result")
	}
	var trip domain.Trip
	if err := json.Unmarshal(snapshot, &trip); err != nil {
		return domain.Trip{}, false, fmt.Errorf("decode idempotency snapshot: %w", err)
	}
	return trip, true, nil
}

func completeIdempotencyKey(ctx context.Context, tx pgx.Tx, key *domain.IdempotencyKey, trip domain.Trip) error {
	snapshot, err := json.Marshal(trip)
	if err != nil {
		return fmt.Errorf("encode idempotency snapshot: %w", err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE idempotency_keys
		   SET trip_id = $3, trip_snapshot = $4
		 WHERE scope = $1 AND idempotency_key = $2`,
		key.Scope, key.Key, trip.ID, snapshot)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	return nil
}

func insertTrip(ctx context.Context, tx pgx.Tx, trip domain.Trip) (domain.Trip, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO trips (id, trip_number, source, destination, driver_name, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now(), now())
		RETURNING `+tripColumns,
		trip.ID, trip.TripNumber, trip.Source, trip.Destination, trip.DriverName, trip.Status)
	created, err := scanTrip(row)
	if err != nil {
		return domain.Trip{}, fmt.Errorf("insert trip: %w", err)
	}
	return created, nil
}

// GetByID returns the trip or domain.ErrTripNotFound.
func (r *TripRepository) GetByID(ctx context.Context, id uuid.UUID) (domain.Trip, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+tripColumns+` FROM trips WHERE id = $1`, id)
	trip, err := scanTrip(row)
	if err != nil {
		return domain.Trip{}, mapError(err)
	}
	return trip, nil
}

// List returns one page of trips, newest first, and the total match count.
// Both queries run in one REPEATABLE READ snapshot so the total and the page
// always agree, even while trips are being created concurrently.
func (r *TripRepository) List(ctx context.Context, f domain.ListFilter) (domain.TripPage, error) {
	// Two fixed statements rather than "($1 IS NULL OR status = $1)": pgx
	// caches prepared statements, and a generic plan for that predicate could
	// not use the (status, created_at, id) index. No SQL is built from input.
	countSQL := `SELECT count(*) FROM trips`
	listSQL := `SELECT ` + tripColumns + ` FROM trips ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`
	args := []any{f.PageSize, f.Offset()}
	var countArgs []any
	if f.Status != nil {
		countSQL = `SELECT count(*) FROM trips WHERE status = $1`
		listSQL = `SELECT ` + tripColumns + ` FROM trips WHERE status = $1 ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`
		countArgs = []any{string(*f.Status)}
		args = append(countArgs, args...)
	}

	var page domain.TripPage
	err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, countSQL, countArgs...).Scan(&page.TotalItems); err != nil {
			return fmt.Errorf("count trips: %w", err)
		}

		rows, err := tx.Query(ctx, listSQL, args...)
		if err != nil {
			return fmt.Errorf("list trips: %w", err)
		}
		page.Trips, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Trip, error) {
			return scanTrip(row)
		})
		if err != nil {
			return fmt.Errorf("scan trips: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.TripPage{}, mapError(err)
	}
	return page, nil
}

// UpdateStatus changes a trip's status under a row lock.
//
// The transaction locks the row with SELECT ... FOR UPDATE, validates the
// transition against the *persisted* status while holding the lock, and only
// then writes. A concurrent update of the same trip blocks on the lock until
// this transaction commits, then re-reads the row and validates against the
// new status, so no request can act on stale state. Locks are row-level and
// held by PostgreSQL, so this holds across any number of API instances.
func (r *TripRepository) UpdateStatus(ctx context.Context, id uuid.UUID, to domain.Status) (domain.Trip, error) {
	var updated domain.Trip
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var current domain.Status
		err := tx.QueryRow(ctx, `SELECT status FROM trips WHERE id = $1 FOR UPDATE`, id).Scan(&current)
		if err != nil {
			return err
		}
		if err := domain.ValidateTransition(current, to); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			UPDATE trips
			   SET status = $2, updated_at = greatest(now(), updated_at)
			 WHERE id = $1
			RETURNING `+tripColumns,
			id, to)
		updated, err = scanTrip(row)
		return err
	})
	if err != nil {
		return domain.Trip{}, mapError(err)
	}
	return updated, nil
}

// Delete physically removes the trip, or returns domain.ErrTripNotFound if no
// row was deleted.
func (r *TripRepository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM trips WHERE id = $1`, id)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrTripNotFound
	}
	return nil
}

// Ping reports whether the database is reachable.
func (r *TripRepository) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}

func scanTrip(row pgx.Row) (domain.Trip, error) {
	var t domain.Trip
	err := row.Scan(&t.ID, &t.TripNumber, &t.Source, &t.Destination, &t.DriverName, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	t.CreatedAt = t.CreatedAt.UTC()
	t.UpdatedAt = t.UpdatedAt.UTC()
	return t, err
}

// mapError translates driver errors into domain errors. Anything it does not
// recognise is returned wrapped and is reported to clients as an internal error.
func mapError(err error) error {
	var pgErr *pgconn.PgError
	var connErr *pgconn.ConnectError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.ErrTripNotFound
	case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "trips_trip_number_key":
		return domain.ErrTripNumberExists
	case errors.As(err, &connErr), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", domain.ErrUnavailable, err)
	case errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "57014"):
		// lock_timeout (lock_not_available) or statement_timeout (query_canceled):
		// the row is busy or the database is slow; the client may retry.
		return fmt.Errorf("%w: %w", domain.ErrUnavailable, err)
	default:
		// Domain errors (TransitionError, ErrIdempotencyKeyReused) pass through
		// unchanged; errors.Is/As still match wrapped driver errors.
		return err
	}
}
