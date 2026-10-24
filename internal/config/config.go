// Package config loads and validates runtime configuration from environment
// variables. Nothing is read from files; .env.example documents the variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully validated application configuration.
type Config struct {
	HTTPAddr           string
	DatabaseURL        string
	LogLevel           slog.Level
	LogFormat          string // "json" or "text"
	ReadHeaderTimeout  time.Duration
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	IdleTimeout        time.Duration
	RequestTimeout     time.Duration
	ShutdownTimeout    time.Duration
	MaxBodyBytes       int64
	DBMaxConns         int32
	DBConnectTimeout   time.Duration
	DBStatementTimeout time.Duration
	DBLockTimeout      time.Duration
	IdempotencyTTL     time.Duration
}

// Load reads configuration using os.Getenv.
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom reads configuration through getenv, which makes it testable.
// Every problem is reported at once rather than one per restart.
func LoadFrom(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	cfg := Config{
		HTTPAddr:           p.str("HTTP_ADDR", ":8080"),
		DatabaseURL:        p.str("DATABASE_URL", ""),
		LogLevel:           p.level("LOG_LEVEL", slog.LevelInfo),
		LogFormat:          strings.ToLower(p.str("LOG_FORMAT", "json")),
		ReadHeaderTimeout:  p.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		ReadTimeout:        p.duration("HTTP_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:       p.duration("HTTP_WRITE_TIMEOUT", 15*time.Second),
		IdleTimeout:        p.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		RequestTimeout:     p.duration("REQUEST_TIMEOUT", 10*time.Second),
		ShutdownTimeout:    p.duration("SHUTDOWN_TIMEOUT", 20*time.Second),
		MaxBodyBytes:       p.int64("MAX_BODY_BYTES", 64<<10),
		DBMaxConns:         int32(p.int64("DB_MAX_CONNS", 10)),
		DBConnectTimeout:   p.duration("DB_CONNECT_TIMEOUT", 30*time.Second),
		DBStatementTimeout: p.duration("DB_STATEMENT_TIMEOUT", 5*time.Second),
		DBLockTimeout:      p.duration("DB_LOCK_TIMEOUT", 2*time.Second),
		IdempotencyTTL:     p.duration("IDEMPOTENCY_TTL", 24*time.Hour),
	}

	if cfg.DatabaseURL == "" {
		p.fail("DATABASE_URL is required")
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		p.fail("LOG_FORMAT must be json or text")
	}
	for name, d := range map[string]time.Duration{
		"HTTP_READ_HEADER_TIMEOUT": cfg.ReadHeaderTimeout,
		"HTTP_READ_TIMEOUT":        cfg.ReadTimeout,
		"HTTP_WRITE_TIMEOUT":       cfg.WriteTimeout,
		"HTTP_IDLE_TIMEOUT":        cfg.IdleTimeout,
		"REQUEST_TIMEOUT":          cfg.RequestTimeout,
		"SHUTDOWN_TIMEOUT":         cfg.ShutdownTimeout,
		"DB_CONNECT_TIMEOUT":       cfg.DBConnectTimeout,
		"DB_STATEMENT_TIMEOUT":     cfg.DBStatementTimeout,
		"DB_LOCK_TIMEOUT":          cfg.DBLockTimeout,
		"IDEMPOTENCY_TTL":          cfg.IdempotencyTTL,
	} {
		if d <= 0 {
			p.fail(name + " must be positive")
		}
	}
	// A handler must give up (and answer) before the server kills the connection.
	if cfg.RequestTimeout >= cfg.WriteTimeout {
		p.fail("REQUEST_TIMEOUT must be less than HTTP_WRITE_TIMEOUT")
	}
	// PostgreSQL must give up on a query (and leave any lock queue) before
	// the handler's context expires and abandons the connection.
	if cfg.DBStatementTimeout >= cfg.RequestTimeout {
		p.fail("DB_STATEMENT_TIMEOUT must be less than REQUEST_TIMEOUT")
	}
	if cfg.DBLockTimeout > cfg.DBStatementTimeout {
		p.fail("DB_LOCK_TIMEOUT must not exceed DB_STATEMENT_TIMEOUT")
	}
	if cfg.MaxBodyBytes < 1024 {
		p.fail("MAX_BODY_BYTES must be at least 1024")
	}
	if cfg.DBMaxConns < 1 {
		p.fail("DB_MAX_CONNS must be at least 1")
	}

	if len(p.errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(p.errs...))
	}
	return cfg, nil
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(msg string) { p.errs = append(p.errs, errors.New(msg)) }

func (p *parser) str(key, def string) string {
	if v := strings.TrimSpace(p.getenv(key)); v != "" {
		return v
	}
	return def
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.fail(fmt.Sprintf("%s: invalid duration %q", key, v))
		return def
	}
	return d
}

func (p *parser) int64(key string, def int64) int64 {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		p.fail(fmt.Sprintf("%s: invalid integer %q", key, v))
		return def
	}
	return n
}

func (p *parser) level(key string, def slog.Level) slog.Level {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail(fmt.Sprintf("%s: invalid level %q (use debug, info, warn or error)", key, v))
		return def
	}
	return l
}
