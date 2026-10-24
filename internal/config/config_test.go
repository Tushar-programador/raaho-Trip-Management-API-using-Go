package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadFrom(env(map[string]string{"DATABASE_URL": "postgres://localhost/db"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.RequestTimeout != 10*time.Second || cfg.MaxBodyBytes != 64<<10 || cfg.LogFormat != "json" || cfg.IdempotencyTTL != 24*time.Hour {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{
		"HTTP_READ_TIMEOUT": "soon",
		"LOG_FORMAT":        "xml",
		"DB_MAX_CONNS":      "0",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"DATABASE_URL is required", "HTTP_READ_TIMEOUT", "LOG_FORMAT", "DB_MAX_CONNS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDatabaseTimeoutsMustNest(t *testing.T) {
	cases := map[string]map[string]string{
		"DB_STATEMENT_TIMEOUT": {"DB_STATEMENT_TIMEOUT": "10s", "REQUEST_TIMEOUT": "10s"},
		"DB_LOCK_TIMEOUT":      {"DB_LOCK_TIMEOUT": "6s", "DB_STATEMENT_TIMEOUT": "5s"},
	}
	for want, vars := range cases {
		vars["DATABASE_URL"] = "postgres://localhost/db"
		_, err := LoadFrom(env(vars))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: expected %s error, got %v", vars, want, err)
		}
	}
}

func TestShutdownDrainDelay(t *testing.T) {
	base := func(delay string) map[string]string {
		return map[string]string{"DATABASE_URL": "postgres://localhost/db", "SHUTDOWN_TIMEOUT": "10s", "SHUTDOWN_DRAIN_DELAY": delay}
	}
	if cfg, err := LoadFrom(env(base("5s"))); err != nil || cfg.ShutdownDrainDelay != 5*time.Second {
		t.Errorf("5s drain delay: %v, %v", cfg.ShutdownDrainDelay, err)
	}
	if cfg, err := LoadFrom(env(base("0s"))); err != nil || cfg.ShutdownDrainDelay != 0 {
		t.Errorf("drain delay 0 must be allowed: %v", err)
	}
	for _, bad := range []string{"10s", "-1s"} {
		if _, err := LoadFrom(env(base(bad))); err == nil || !strings.Contains(err.Error(), "SHUTDOWN_DRAIN_DELAY") {
			t.Errorf("drain delay %s: expected error, got %v", bad, err)
		}
	}
}

func TestIdempotencyTTLMinimum(t *testing.T) {
	for ttl, wantErr := range map[string]bool{"500ns": true, "999ms": true, "1s": false, "24h": false} {
		_, err := LoadFrom(env(map[string]string{"DATABASE_URL": "postgres://localhost/db", "IDEMPOTENCY_TTL": ttl}))
		if gotErr := err != nil && strings.Contains(err.Error(), "IDEMPOTENCY_TTL"); gotErr != wantErr {
			t.Errorf("IDEMPOTENCY_TTL=%s: error = %v, want error %v", ttl, err, wantErr)
		}
	}
}

func TestRequestTimeoutMustBeBelowWriteTimeout(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{
		"DATABASE_URL":       "postgres://localhost/db",
		"REQUEST_TIMEOUT":    "20s",
		"HTTP_WRITE_TIMEOUT": "15s",
	}))
	if err == nil || !strings.Contains(err.Error(), "REQUEST_TIMEOUT") {
		t.Fatalf("expected REQUEST_TIMEOUT error, got %v", err)
	}
}
