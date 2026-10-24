// Package database owns the PostgreSQL connection pool and schema migrations.
package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// PoolOptions tunes the connection pool. Zero timeouts keep the server default
// (no limit), which is what schema migrations need.
type PoolOptions struct {
	MaxConns       int32
	ConnectTimeout time.Duration
	// StatementTimeout and LockTimeout are enforced by PostgreSQL itself. A
	// cancelled Go context only closes the client socket, and a backend that
	// is queued on a row lock does not notice that, so without a server-side
	// lock_timeout an abandoned waiter would stay queued on the server.
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

// Connect creates a pool and waits until PostgreSQL answers a ping, retrying
// with backoff until opts.ConnectTimeout elapses. This tolerates the database
// starting slightly after the API without hiding a real outage.
func Connect(ctx context.Context, url string, opts PoolOptions, log *slog.Logger) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// The parse error can echo the URL, which may contain a password.
		return nil, errors.New("database: invalid DATABASE_URL")
	}
	cfg.MaxConns = opts.MaxConns
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	if opts.StatementTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	}
	if opts.LockTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["lock_timeout"] = strconv.FormatInt(opts.LockTimeout.Milliseconds(), 10)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()
	backoff := 250 * time.Millisecond
	for {
		pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pingCtx)
		pingCancel()
		if err == nil {
			return pool, nil
		}
		log.Warn("database not ready, retrying", "error", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("database: not reachable within %s: %w", opts.ConnectTimeout, err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// LatestVersion returns the highest migration version present in fsys.
func LatestVersion(fsys fs.FS) (uint, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, err
	}
	var latest uint
	for _, e := range entries {
		var v uint
		if _, err := fmt.Sscanf(e.Name(), "%d_", &v); err == nil && strings.HasSuffix(e.Name(), ".up.sql") {
			latest = max(latest, v)
		}
	}
	if latest == 0 {
		return 0, errors.New("database: no migrations found")
	}
	return latest, nil
}

// CheckSchema verifies that migrations have been applied up to want and that
// the last migration did not fail half-way. The API refuses to start
// otherwise, so a missing migration step is a visible startup failure rather
// than a stream of 500s.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool, want uint) error {
	var version int64
	var dirty bool
	err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	if err != nil {
		return fmt.Errorf("database: read schema version (have migrations been run?): %w", err)
	}
	if dirty {
		return fmt.Errorf("database: schema version %d is dirty; fix the failed migration and force the version", version)
	}
	if version < int64(want) {
		return fmt.Errorf("database: schema version %d is older than required version %d; run migrations", version, want)
	}
	return nil
}

// IsPrivilegedRole reports whether the connected role can change the schema:
// it is a superuser, owns the trips table, or is a member of the owner role.
// The API only needs row access, so a true result means it is running with
// far more privilege than necessary.
func IsPrivilegedRole(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var privileged bool
	err := pool.QueryRow(ctx,
		`SELECT pg_has_role(current_user, relowner, 'MEMBER') FROM pg_class WHERE oid = 'public.trips'::regclass`,
	).Scan(&privileged)
	if err != nil {
		return false, fmt.Errorf("database: check role privileges: %w", err)
	}
	return privileged, nil
}

// NewMigrator builds a golang-migrate instance that applies the migrations in
// fsys over an existing pool. golang-migrate takes a PostgreSQL advisory lock,
// so concurrent runs cannot apply the same migration twice.
func NewMigrator(pool *pgxpool.Pool, fsys fs.FS) (*migrate.Migrate, error) {
	src, err := iofs.New(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: open source: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	driver, err := pgx.WithInstance(db, &pgx.Config{})
	if err != nil {
		return nil, fmt.Errorf("migrate: open driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return nil, fmt.Errorf("migrate: init: %w", err)
	}
	return m, nil
}
