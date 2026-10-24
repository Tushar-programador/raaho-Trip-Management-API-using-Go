package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Tushar-programador/trip-management-api/internal/platform/database"
)

// TestAPIRoleCannotChangeSchema: the role the API connects as must be limited
// to row access. Every statement below must fail with insufficient_privilege
// (42501). Row access itself is exercised by every other integration test,
// since they all run through the API on this role.
func TestAPIRoleCannotChangeSchema(t *testing.T) {
	setup(t)
	if appPool == nil {
		t.Skip("TEST_APP_DATABASE_URL not set")
	}
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP TABLE trips`,
		`ALTER TABLE trips ADD COLUMN injected text`,
		`ALTER TABLE trips DROP CONSTRAINT trips_status_check`,
		`TRUNCATE trips`,
		`CREATE TABLE injected (id int)`,
		`CREATE INDEX injected_idx ON trips (driver_name)`,
		`DELETE FROM schema_migrations`,
		`UPDATE schema_migrations SET dirty = true`,
	} {
		_, err := appPool.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: want permission denied (42501), got %v", stmt, err)
		}
	}
}

func TestIsPrivilegedRole(t *testing.T) {
	setup(t)
	ctx := context.Background()

	if privileged, err := database.IsPrivilegedRole(ctx, pool); err != nil || !privileged {
		t.Errorf("owner role: privileged = %v, err = %v; want true", privileged, err)
	}
	if appPool == nil {
		t.Skip("TEST_APP_DATABASE_URL not set")
	}
	if privileged, err := database.IsPrivilegedRole(ctx, appPool); err != nil || privileged {
		t.Errorf("API role: privileged = %v, err = %v; want false", privileged, err)
	}
}
