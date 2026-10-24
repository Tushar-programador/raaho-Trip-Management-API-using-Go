// Package tests contains HTTP integration tests that run the real router,
// service and repository against a real PostgreSQL database.
//
// They require TEST_DATABASE_URL (the schema owner) and are skipped without
// it. The database is migrated and then TRUNCATED between tests, so never
// point it at real data. When TEST_APP_DATABASE_URL is also set, the API under
// test connects as that (least-privilege) role, exactly as in production:
//
//	TEST_DATABASE_URL=postgres://trips:trips@localhost:5432/trips_test?sslmode=disable \
//	TEST_APP_DATABASE_URL=postgres://trip_api:trip_api@localhost:5432/trips_test?sslmode=disable \
//	go test ./tests/...
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
	"github.com/Tushar-programador/trip-management-api/internal/handler"
	"github.com/Tushar-programador/trip-management-api/internal/platform/database"
	"github.com/Tushar-programador/trip-management-api/internal/repository"
	"github.com/Tushar-programador/trip-management-api/internal/service"
	"github.com/Tushar-programador/trip-management-api/migrations"
)

const lockTimeout = 2 * time.Second

var (
	pool    *pgxpool.Pool // owner role: truncation and direct assertions
	appPool *pgxpool.Pool // the API's connection; nil when TEST_APP_DATABASE_URL is unset
	baseURL string        // the API under test
	client  = &http.Client{Timeout: 30 * time.Second}
)

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		fmt.Println("TEST_DATABASE_URL not set: integration tests will be skipped")
		os.Exit(m.Run())
	}
	os.Exit(runWithDatabase(m, url))
}

func runWithDatabase(m *testing.M, url string) int {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := migrateUp(ctx, url, log); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}

	// Enough connections that concurrent requests really contend inside
	// PostgreSQL instead of queueing for a pool slot. A short lock timeout
	// keeps TestStatusUpdateLockTimeout fast; it is still far longer than any
	// lock wait in the other tests.
	opts := database.PoolOptions{
		MaxConns:         32,
		ConnectTimeout:   15 * time.Second,
		StatementTimeout: 10 * time.Second,
		LockTimeout:      lockTimeout,
	}
	var err error
	pool, err = database.Connect(ctx, url, opts, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 1
	}
	defer pool.Close()

	apiPool := pool
	if appURL := os.Getenv("TEST_APP_DATABASE_URL"); appURL != "" {
		appPool, err = database.Connect(ctx, appURL, opts, log)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connect as API role:", err)
			return 1
		}
		defer appPool.Close()
		apiPool = appPool
	} else {
		fmt.Println("TEST_APP_DATABASE_URL not set: the API under test connects as the owner role")
	}

	svc := service.NewTripService(repository.NewTripRepository(apiPool), time.Hour)
	srv := httptest.NewServer(handler.NewRouter(svc, log, handler.RouterOptions{
		RequestTimeout: 20 * time.Second,
		MaxBodyBytes:   64 << 10,
	}))
	defer srv.Close()
	baseURL = srv.URL

	return m.Run()
}

func migrateUp(ctx context.Context, url string, log *slog.Logger) error {
	p, err := database.Connect(ctx, url, database.PoolOptions{MaxConns: 2, ConnectTimeout: 15 * time.Second}, log)
	if err != nil {
		return err
	}
	defer p.Close()
	mg, err := database.NewMigrator(p, migrations.FS)
	if err != nil {
		return err
	}
	defer mg.Close()
	if err := mg.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// setup skips the test without a database and gives it empty tables.
func setup(t *testing.T) {
	t.Helper()
	if pool == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE trips, idempotency_keys`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// ---- HTTP helpers ---------------------------------------------------------

type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r response) trip(t *testing.T) domain.Trip {
	t.Helper()
	var trip domain.Trip
	if err := json.Unmarshal(r.Body, &trip); err != nil {
		t.Fatalf("decode trip: %v: %s", err, r.Body)
	}
	return trip
}

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Details   []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"details"`
}

func (r response) errorBody(t *testing.T) apiError {
	t.Helper()
	var env struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil || env.Error.Code == "" {
		t.Fatalf("not an error envelope (%v): %s", err, r.Body)
	}
	if env.Error.RequestID == "" {
		t.Errorf("error without request_id: %s", r.Body)
	}
	return env.Error
}

type listBody struct {
	Data       []domain.Trip      `json:"data"`
	Pagination handler.Pagination `json:"pagination"`
}

func (r response) list(t *testing.T) listBody {
	t.Helper()
	var lb listBody
	if err := json.Unmarshal(r.Body, &lb); err != nil {
		t.Fatalf("decode list: %v: %s", err, r.Body)
	}
	return lb
}

// send performs a request. It is safe to call from multiple goroutines; it
// reports failures with t.Errorf so it never calls FailNow off the test goroutine.
func send(t *testing.T, method, path string, body any, headers map[string]string) response {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = bytes.NewBufferString(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			t.Errorf("marshal: %v", err)
			return response{}
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, baseURL+path, rdr)
	if err != nil {
		t.Errorf("new request: %v", err)
		return response{}
	}
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Errorf("%s %s: %v", method, path, err)
		return response{}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("read body: %v", err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: data}
}

func tripInput(number string) map[string]string {
	return map[string]string{
		"trip_number": number,
		"source":      "Mumbai",
		"destination": "Pune",
		"driver_name": "Ravi Kumar",
	}
}

func mustCreate(t *testing.T, number string) domain.Trip {
	t.Helper()
	r := send(t, http.MethodPost, "/trips", tripInput(number), nil)
	if r.Status != http.StatusCreated {
		t.Fatalf("create %s: status %d: %s", number, r.Status, r.Body)
	}
	return r.trip(t)
}

func mustPatch(t *testing.T, id fmt.Stringer, status domain.Status) domain.Trip {
	t.Helper()
	r := send(t, http.MethodPatch, "/trips/"+id.String()+"/status", map[string]string{"status": string(status)}, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("patch %s -> %s: status %d: %s", id, status, r.Status, r.Body)
	}
	return r.trip(t)
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func dbStatus(t *testing.T, id fmt.Stringer) domain.Status {
	t.Helper()
	var s domain.Status
	if err := pool.QueryRow(context.Background(), `SELECT status FROM trips WHERE id = $1`, id.String()).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}
