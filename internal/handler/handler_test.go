package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
	"github.com/Tushar-programador/trip-management-api/internal/service"
)

// fakeService records calls and returns canned results; it lets these tests
// exercise HTTP parsing and error mapping without PostgreSQL.
type fakeService struct {
	createErr  error
	created    domain.Trip
	replayed   bool
	gotKey     string
	gotInput   domain.CreateTripInput
	getErr     error
	panicOnGet bool
	readyErr   error
	calls      atomic.Int32
}

func (f *fakeService) Create(_ context.Context, in domain.CreateTripInput, key string) (service.CreateResult, error) {
	f.calls.Add(1)
	f.gotInput, f.gotKey = in, key
	return service.CreateResult{Trip: f.created, Replayed: f.replayed}, f.createErr
}

func (f *fakeService) Get(context.Context, uuid.UUID) (domain.Trip, error) {
	f.calls.Add(1)
	if f.panicOnGet {
		panic("boom: secret internal detail")
	}
	return f.created, f.getErr
}

func (f *fakeService) List(context.Context, domain.ListFilter) (domain.TripPage, error) {
	f.calls.Add(1)
	return domain.TripPage{}, nil
}

func (f *fakeService) UpdateStatus(context.Context, uuid.UUID, string) (domain.Trip, error) {
	f.calls.Add(1)
	return f.created, nil
}

func (f *fakeService) Delete(context.Context, uuid.UUID) error {
	f.calls.Add(1)
	return nil
}

func (f *fakeService) Ready(context.Context) error { return f.readyErr }

func newTestRouter(svc TripService) http.Handler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRouter(svc, log, RouterOptions{RequestTimeout: 5 * time.Second, MaxBodyBytes: 1024})
}

func do(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func readError(t *testing.T, rec *httptest.ResponseRecorder) errorDetail {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error envelope: %v: %s", err, rec.Body)
	}
	if body.Error.RequestID == "" {
		t.Error("error response has no request_id")
	}
	return body.Error
}

func TestCreateRejectsBadBodies(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{"malformed", `{"trip_number":`, "application/json", 400, CodeInvalidJSON},
		{"syntax error", `{trip_number: "x"}`, "application/json", 400, CodeInvalidJSON},
		{"empty body", ``, "application/json", 400, CodeInvalidJSON},
		{"unknown field", `{"trip_number":"T","source":"A","destination":"B","driver_name":"C","status":"COMPLETED"}`, "application/json", 400, CodeInvalidJSON},
		{"client-supplied id", `{"id":"x","trip_number":"T","source":"A","destination":"B","driver_name":"C"}`, "application/json", 400, CodeInvalidJSON},
		{"wrong type", `{"trip_number":123,"source":"A","destination":"B","driver_name":"C"}`, "application/json", 400, CodeInvalidJSON},
		{"array body", `[]`, "application/json", 400, CodeInvalidJSON},
		{"two objects", `{"trip_number":"T"} {"trip_number":"U"}`, "application/json", 400, CodeInvalidJSON},
		{"no content type", `{"trip_number":"T"}`, "", 415, CodeUnsupportedMediaType},
		{"form content type", `trip_number=T`, "application/x-www-form-urlencoded", 415, CodeUnsupportedMediaType},
		{"oversized", `{"trip_number":"` + strings.Repeat("a", 2048) + `"}`, "application/json", 413, CodePayloadTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &fakeService{}
			req := httptest.NewRequest(http.MethodPost, "/trips", strings.NewReader(c.body))
			if c.contentType != "" {
				req.Header.Set("Content-Type", c.contentType)
			}
			rec := httptest.NewRecorder()
			newTestRouter(svc).ServeHTTP(rec, req)

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.wantStatus, rec.Body)
			}
			if got := readError(t, rec); got.Code != c.wantCode {
				t.Errorf("code = %q, want %q", got.Code, c.wantCode)
			}
			if svc.calls.Load() != 0 {
				t.Error("service must not be called for a rejected body")
			}
		})
	}
}

func TestCreateSuccessAndReplayHeaders(t *testing.T) {
	trip := domain.Trip{ID: uuid.Must(uuid.NewV7()), TripNumber: "T-1", Status: domain.StatusCreated}
	svc := &fakeService{created: trip, replayed: true}
	body := `{"trip_number":"T-1","source":"A","destination":"B","driver_name":"C"}`
	rec := do(t, newTestRouter(svc), http.MethodPost, "/trips", body, map[string]string{"Idempotency-Key": "abc-123"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if svc.gotKey != "abc-123" {
		t.Errorf("idempotency key passed to service = %q", svc.gotKey)
	}
	if rec.Header().Get("Location") != "/trips/"+trip.ID.String() {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	if rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("missing Idempotent-Replayed header on replay")
	}
	var got domain.Trip
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID != trip.ID {
		t.Errorf("body = %s (err %v)", rec.Body, err)
	}
}

func TestCreateRejectsEmptyIdempotencyKeyHeader(t *testing.T) {
	svc := &fakeService{}
	req := httptest.NewRequest(http.MethodPost, "/trips", strings.NewReader(`{"trip_number":"T","source":"A","destination":"B","driver_name":"C"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header["Idempotency-Key"] = []string{""}
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || readError(t, rec).Code != CodeValidation {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if svc.calls.Load() != 0 {
		t.Error("service must not be called")
	}
}

func TestInvalidUUIDs(t *testing.T) {
	ids := []string{
		"not-a-uuid",
		"123",
		"0192f3a45b6c7d8e9f0123456789abcd",       // unhyphenated
		"{0192f3a4-5b6c-7d8e-9f01-23456789abcd}", // braced
		"urn:uuid:0192f3a4-5b6c-7d8e-9f01-23456789abcd", // URN
		"0192f3a4-5b6c-7d8e-9f01-23456789abcz",
	}
	for _, id := range ids {
		for _, req := range []struct{ method, path, body string }{
			{http.MethodGet, "/trips/" + id, ""},
			{http.MethodDelete, "/trips/" + id, ""},
			{http.MethodPatch, "/trips/" + id + "/status", `{"status":"ASSIGNED"}`},
		} {
			svc := &fakeService{}
			rec := do(t, newTestRouter(svc), req.method, req.path, req.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s %s: status = %d, want 400", req.method, req.path, rec.Code)
				continue
			}
			if e := readError(t, rec); e.Code != CodeValidation || e.Details[0].Field != "id" {
				t.Errorf("%s %s: error = %+v", req.method, req.path, e)
			}
			if svc.calls.Load() != 0 {
				t.Errorf("%s %s: service must not be called", req.method, req.path)
			}
		}
	}
}

func TestRequestIDPropagation(t *testing.T) {
	h := newTestRouter(&fakeService{getErr: domain.ErrTripNotFound})
	path := "/trips/" + uuid.NewString()

	rec := do(t, h, http.MethodGet, path, "", map[string]string{"X-Request-ID": "client-req-42"})
	if rec.Header().Get("X-Request-ID") != "client-req-42" || readError(t, rec).RequestID != "client-req-42" {
		t.Errorf("inbound request ID not reused: header=%q body=%s", rec.Header().Get("X-Request-ID"), rec.Body)
	}

	rec = do(t, h, http.MethodGet, path, "", map[string]string{"X-Request-ID": "bad id\nwith newline"})
	if got := rec.Header().Get("X-Request-ID"); got == "" || strings.Contains(got, " ") {
		t.Errorf("unsafe inbound request ID should be replaced, got %q", got)
	}
}

func TestPanicRecoveryHidesDetails(t *testing.T) {
	rec := do(t, newTestRouter(&fakeService{panicOnGet: true}), http.MethodGet, "/trips/"+uuid.NewString(), "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if readError(t, rec).Code != CodeInternal || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("panic response leaked details: %s", rec.Body)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	h := newTestRouter(&fakeService{})
	if rec := do(t, h, http.MethodGet, "/nope", "", nil); rec.Code != 404 || readError(t, rec).Code != CodeRouteNotFound {
		t.Errorf("unknown route: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodPut, "/trips", "", nil); rec.Code != 405 || readError(t, rec).Code != CodeMethodNotAllowed {
		t.Errorf("wrong method: %d %s", rec.Code, rec.Body)
	}
}

func TestReadiness(t *testing.T) {
	svc := &fakeService{}
	var draining atomic.Bool
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewRouter(svc, log, RouterOptions{RequestTimeout: time.Second, MaxBodyBytes: 1024, Draining: &draining})

	if rec := do(t, h, http.MethodGet, "/readyz", "", nil); rec.Code != 200 {
		t.Errorf("ready: %d", rec.Code)
	}
	svc.readyErr = errors.New("connection refused")
	if rec := do(t, h, http.MethodGet, "/readyz", "", nil); rec.Code != 503 || strings.Contains(rec.Body.String(), "refused") {
		t.Errorf("db down: %d %s", rec.Code, rec.Body)
	}
	svc.readyErr = nil
	draining.Store(true)
	if rec := do(t, h, http.MethodGet, "/readyz", "", nil); rec.Code != 503 {
		t.Errorf("draining: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/healthz", "", nil); rec.Code != 200 {
		t.Errorf("liveness must not depend on draining/db: %d", rec.Code)
	}
}

func TestMapError(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{&domain.ValidationError{Fields: []domain.FieldError{{Field: "source", Message: "is required"}}}, 400, CodeValidation},
		{&domain.InvalidStatusError{Value: "DONE"}, 400, CodeInvalidStatus},
		{&domain.TransitionError{From: domain.StatusCreated, To: domain.StatusCompleted}, 409, CodeInvalidStatusTransition},
		{domain.ErrTripNotFound, 404, CodeTripNotFound},
		{domain.ErrTripNumberExists, 409, CodeTripNumberExists},
		{domain.ErrIdempotencyKeyReused, 409, CodeIdempotencyKeyReused},
		{fmt.Errorf("%w: dial tcp: refused", domain.ErrUnavailable), 503, CodeServiceUnavailable},
		{fmt.Errorf("wrapped: %w", domain.ErrTripNotFound), 404, CodeTripNotFound},
		{errors.New(`ERROR: relation "trips" does not exist (SQLSTATE 42P01)`), 500, CodeInternal},
	}
	for _, c := range cases {
		status, detail := mapError(c.err)
		if status != c.wantStatus || detail.Code != c.wantCode {
			t.Errorf("mapError(%v) = %d %s, want %d %s", c.err, status, detail.Code, c.wantStatus, c.wantCode)
		}
		if status == 500 && strings.Contains(detail.Message, "SQLSTATE") {
			t.Errorf("internal error leaked: %q", detail.Message)
		}
	}
}
