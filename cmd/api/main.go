// Command api runs the Trip Management HTTP API.
//
// Usage:
//
//	api              start the server
//	api healthcheck  probe /readyz on the local server (for container health checks)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Tushar-programador/trip-management-api/internal/config"
	"github.com/Tushar-programador/trip-management-api/internal/handler"
	"github.com/Tushar-programador/trip-management-api/internal/platform/database"
	"github.com/Tushar-programador/trip-management-api/internal/platform/logger"
	"github.com/Tushar-programador/trip-management-api/internal/repository"
	"github.com/Tushar-programador/trip-management-api/internal/service"
	"github.com/Tushar-programador/trip-management-api/migrations"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger.New(os.Stdout, cfg.LogFormat, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL, database.PoolOptions{
		MaxConns:         cfg.DBMaxConns,
		ConnectTimeout:   cfg.DBConnectTimeout,
		StatementTimeout: cfg.DBStatementTimeout,
		LockTimeout:      cfg.DBLockTimeout,
	}, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Refuse to serve against a schema that is missing migrations. Migrations
	// are applied by the separate migrate command, never by API replicas.
	want, err := database.LatestVersion(migrations.FS)
	if err != nil {
		return err
	}
	if err := database.CheckSchema(ctx, pool, want); err != nil {
		return err
	}
	if privileged, err := database.IsPrivilegedRole(ctx, pool); err != nil {
		return err
	} else if privileged {
		log.Warn("the API is connected as a role that can alter or drop the schema; " +
			"connect it as the least-privilege trip_api role (see README, Database roles)")
	}

	var draining atomic.Bool
	svc := service.NewTripService(repository.NewTripRepository(pool), cfg.IdempotencyTTL)
	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: handler.NewRouter(svc, repository.NewClientRepository(pool), log, handler.RouterOptions{
			RequestTimeout: cfg.RequestTimeout,
			MaxBodyBytes:   cfg.MaxBodyBytes,
			Draining:       &draining,
		}),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr, "schema_version", want)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	stop() // a second signal now terminates immediately
	log.Info("shutdown signal received, draining",
		"drain_delay", cfg.ShutdownDrainDelay.String(), "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// Fail readiness first and keep serving for the drain delay, so load
	// balancers see the 503 and stop routing here before the listener closes.
	draining.Store(true)
	if cfg.ShutdownDrainDelay > 0 {
		time.Sleep(cfg.ShutdownDrainDelay)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}

// healthcheck lets a distroless container (no curl/wget) check itself.
func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid HTTP_ADDR:", err)
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/readyz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
