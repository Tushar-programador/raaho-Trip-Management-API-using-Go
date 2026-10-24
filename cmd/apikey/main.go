// Command apikey manages the API clients allowed to call the Trip API.
//
// Usage:
//
//	apikey create NAME   register a client and print its new key (shown once)
//	apikey list          list clients (keys are never shown, only a prefix)
//	apikey revoke ID     revoke a client's key; takes effect on the next request
//
// It connects with ADMIN_DATABASE_URL (the schema owner): the API's own role
// can only read api_clients. The new key is the only thing written to stdout,
// so it can be captured with KEY=$(apikey create NAME).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/Tushar-programador/trip-management-api/internal/auth"
	"github.com/Tushar-programador/trip-management-api/internal/domain"
	"github.com/Tushar-programador/trip-management-api/internal/platform/database"
	"github.com/Tushar-programador/trip-management-api/internal/platform/logger"
	"github.com/Tushar-programador/trip-management-api/internal/repository"
)

const usage = "usage: apikey create NAME | list | revoke ID"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "apikey:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	url := os.Getenv("ADMIN_DATABASE_URL")
	if url == "" {
		return errors.New("ADMIN_DATABASE_URL is required (the schema owner, not the API role)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	log := logger.New(os.Stderr, "text", 0)
	pool, err := database.Connect(ctx, url, database.PoolOptions{MaxConns: 1, ConnectTimeout: 30 * time.Second}, log)
	if err != nil {
		return err
	}
	defer pool.Close()
	clients := repository.NewClientRepository(pool)

	switch {
	case args[0] == "create" && len(args) == 2:
		return create(ctx, clients, args[1])
	case args[0] == "list" && len(args) == 1:
		return list(ctx, clients)
	case args[0] == "revoke" && len(args) == 2:
		return revoke(ctx, clients, args[1])
	default:
		return errors.New(usage)
	}
}

func create(ctx context.Context, clients *repository.ClientRepository, name string) error {
	name = strings.TrimSpace(name)
	if err := domain.ValidateClientName(name); err != nil {
		return err
	}
	key, err := auth.NewKey()
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	c, err := clients.Create(ctx, id, name, auth.DisplayPrefix(key), auth.HashKey(key))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Created API client %q (id %s).\nStore this key now; it cannot be shown again:\n", c.Name, c.ID)
	fmt.Println(key)
	return nil
}

func list(ctx context.Context, clients *repository.ClientRepository) error {
	all, err := clients.List(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tKEY PREFIX\tCREATED\tREVOKED")
	for _, c := range all {
		revoked := "-"
		if c.RevokedAt != nil {
			revoked = c.RevokedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s…\t%s\t%s\n", c.ID, c.Name, c.KeyPrefix, c.CreatedAt.Format(time.RFC3339), revoked)
	}
	return w.Flush()
}

func revoke(ctx context.Context, clients *repository.ClientRepository, rawID string) error {
	id, err := uuid.Parse(rawID)
	if err != nil {
		return fmt.Errorf("invalid client id %q", rawID)
	}
	c, err := clients.Revoke(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Revoked API client %q (id %s) at %s.\n", c.Name, c.ID, c.RevokedAt.Format(time.RFC3339))
	return nil
}
