// Command migrate applies the embedded schema migrations to DATABASE_URL.
//
// Usage:
//
//	migrate up            apply all pending migrations
//	migrate down          roll back the most recent migration
//	migrate version       print the current schema version
//	migrate force VERSION mark VERSION as applied and clean (after a failed migration)
//
// Run it once per deployment, before starting API replicas. golang-migrate
// holds a PostgreSQL advisory lock while migrating, so accidental concurrent
// runs are serialized rather than corrupting the schema.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/Tushar-programador/trip-management-api/internal/platform/database"
	"github.com/Tushar-programador/trip-management-api/internal/platform/logger"
	"github.com/Tushar-programador/trip-management-api/migrations"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: migrate up|down|version|force VERSION")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}

	log := logger.New(os.Stdout, "text", 0)
	pool, err := database.Connect(context.Background(), url, 2, 30*time.Second, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	m, err := database.NewMigrator(pool, migrations.FS)
	if err != nil {
		return err
	}
	defer m.Close()

	switch args[0] {
	case "up":
		err = m.Up()
	case "down":
		err = m.Steps(-1)
	case "force":
		if len(args) != 2 {
			return errors.New("usage: migrate force VERSION")
		}
		v, convErr := strconv.Atoi(args[1])
		if convErr != nil {
			return fmt.Errorf("invalid version %q", args[1])
		}
		err = m.Force(v)
	case "version":
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}

	version, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		fmt.Println("schema version: none")
	case err != nil:
		return err
	default:
		fmt.Printf("schema version: %d (dirty: %t)\n", version, dirty)
	}
	return nil
}
