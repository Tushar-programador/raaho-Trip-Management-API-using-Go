package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
)

const clientColumns = `id, name, key_prefix, created_at, revoked_at`

// ClientRepository stores API clients. The API itself only calls
// FindActiveByKeyHash, and its database role has nothing but SELECT on
// api_clients. Create, List and Revoke are administrative operations run by
// cmd/apikey as the schema owner.
type ClientRepository struct {
	pool *pgxpool.Pool
}

// NewClientRepository returns a repository backed by pool.
func NewClientRepository(pool *pgxpool.Pool) *ClientRepository {
	return &ClientRepository{pool: pool}
}

// FindActiveByKeyHash returns the non-revoked client owning the key with this
// hash, or domain.ErrUnauthenticated. It is a unique-index lookup.
func (r *ClientRepository) FindActiveByKeyHash(ctx context.Context, keyHash string) (domain.Client, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+clientColumns+` FROM api_clients WHERE key_hash = $1 AND revoked_at IS NULL`, keyHash)
	c, err := scanClient(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Client{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return domain.Client{}, mapError(err)
	}
	return c, nil
}

// Create stores a new client. Only the key's hash and display prefix are
// persisted; the key itself is shown to the operator once and then lost.
func (r *ClientRepository) Create(ctx context.Context, id uuid.UUID, name, keyPrefix, keyHash string) (domain.Client, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO api_clients (id, name, key_prefix, key_hash)
		VALUES ($1, $2, $3, $4)
		RETURNING `+clientColumns,
		id, name, keyPrefix, keyHash)
	c, err := scanClient(row)
	if err != nil {
		return domain.Client{}, fmt.Errorf("create client: %w", mapError(err))
	}
	return c, nil
}

// List returns every client, oldest first, including revoked ones.
func (r *ClientRepository) List(ctx context.Context) ([]domain.Client, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+clientColumns+` FROM api_clients ORDER BY created_at, id`)
	if err != nil {
		return nil, mapError(err)
	}
	clients, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Client, error) {
		return scanClient(row)
	})
	if err != nil {
		return nil, mapError(err)
	}
	return clients, nil
}

// Revoke disables a client's key immediately. Revoking an already revoked
// client keeps the original revocation time.
func (r *ClientRepository) Revoke(ctx context.Context, id uuid.UUID) (domain.Client, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE api_clients SET revoked_at = coalesce(revoked_at, now())
		 WHERE id = $1
		RETURNING `+clientColumns, id)
	c, err := scanClient(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Client{}, domain.ErrClientNotFound
	}
	if err != nil {
		return domain.Client{}, mapError(err)
	}
	return c, nil
}

func scanClient(row pgx.Row) (domain.Client, error) {
	var c domain.Client
	err := row.Scan(&c.ID, &c.Name, &c.KeyPrefix, &c.CreatedAt, &c.RevokedAt)
	c.CreatedAt = c.CreatedAt.UTC()
	if c.RevokedAt != nil {
		t := c.RevokedAt.UTC()
		c.RevokedAt = &t
	}
	return c, err
}
