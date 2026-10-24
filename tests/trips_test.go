package tests

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
)

func TestCreateTrip(t *testing.T) {
	setup(t)
	r := send(t, http.MethodPost, "/trips", map[string]string{
		"trip_number": "  TRIP-1001 ",
		"source":      "Mumbai",
		"destination": "Pune",
		"driver_name": "Ravi Kumar",
	}, nil)
	if r.Status != http.StatusCreated {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
	trip := r.trip(t)

	if trip.ID == uuid.Nil || trip.ID.Version() != 7 {
		t.Errorf("id = %s, want a UUIDv7", trip.ID)
	}
	if trip.TripNumber != "TRIP-1001" {
		t.Errorf("trip_number = %q, want trimmed value", trip.TripNumber)
	}
	if trip.Status != domain.StatusCreated {
		t.Errorf("status = %s", trip.Status)
	}
	if trip.CreatedAt.IsZero() || !trip.CreatedAt.Equal(trip.UpdatedAt) || trip.CreatedAt.Location().String() != "UTC" {
		t.Errorf("timestamps: created_at=%v updated_at=%v", trip.CreatedAt, trip.UpdatedAt)
	}
	if loc := r.Header.Get("Location"); loc != "/trips/"+trip.ID.String() {
		t.Errorf("Location = %q", loc)
	}

	// The response reflects exactly what was persisted.
	got := send(t, http.MethodGet, "/trips/"+trip.ID.String(), nil, nil)
	if got.Status != http.StatusOK || got.trip(t) != trip {
		t.Errorf("GET after create: %d %s, want %+v", got.Status, got.Body, trip)
	}
}

func TestCreateTripValidation(t *testing.T) {
	setup(t)
	r := send(t, http.MethodPost, "/trips", map[string]string{
		"trip_number": "   ",
		"source":      "Mumbai",
		"destination": strings.Repeat("x", domain.MaxDestinationLength+1),
	}, nil)
	if r.Status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
	e := r.errorBody(t)
	if e.Code != "VALIDATION_ERROR" || len(e.Details) != 3 {
		t.Errorf("error = %+v, want 3 field errors (trip_number, destination, driver_name)", e)
	}

	// Clients cannot choose the status (or any server-generated field).
	body := `{"trip_number":"T-1","source":"A","destination":"B","driver_name":"C","status":"COMPLETED"}`
	if r := send(t, http.MethodPost, "/trips", body, nil); r.Status != http.StatusBadRequest || r.errorBody(t).Code != "INVALID_JSON" {
		t.Errorf("client-supplied status: %d %s", r.Status, r.Body)
	}
	if n := countRows(t, `SELECT count(*) FROM trips`); n != 0 {
		t.Errorf("%d trips created by invalid requests", n)
	}
}

func TestMalformedJSON(t *testing.T) {
	setup(t)
	for _, body := range []string{`{"trip_number":`, `not json`, `{"trip_number":"T"}{}`} {
		r := send(t, http.MethodPost, "/trips", body, nil)
		if r.Status != http.StatusBadRequest || r.errorBody(t).Code != "INVALID_JSON" {
			t.Errorf("body %q: %d %s", body, r.Status, r.Body)
		}
	}
	trip := mustCreate(t, "T-JSON")
	r := send(t, http.MethodPatch, "/trips/"+trip.ID.String()+"/status", `{"status":`, nil)
	if r.Status != http.StatusBadRequest || r.errorBody(t).Code != "INVALID_JSON" {
		t.Errorf("patch malformed: %d %s", r.Status, r.Body)
	}
}

func TestDuplicateTripNumber(t *testing.T) {
	setup(t)
	mustCreate(t, "TRIP-DUP")

	r := send(t, http.MethodPost, "/trips", tripInput("TRIP-DUP"), nil)
	if r.Status != http.StatusConflict || r.errorBody(t).Code != "TRIP_NUMBER_EXISTS" {
		t.Fatalf("duplicate: %d %s", r.Status, r.Body)
	}
	if strings.Contains(string(r.Body), "SQLSTATE") || strings.Contains(string(r.Body), "constraint") {
		t.Errorf("database details leaked: %s", r.Body)
	}
	// Surrounding whitespace is trimmed, so this is the same trip number.
	if r := send(t, http.MethodPost, "/trips", tripInput(" TRIP-DUP "), nil); r.Status != http.StatusConflict {
		t.Errorf("whitespace variant: %d", r.Status)
	}
	if n := countRows(t, `SELECT count(*) FROM trips WHERE trip_number = 'TRIP-DUP'`); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestGetTrip(t *testing.T) {
	setup(t)
	trip := mustCreate(t, "T-GET")

	if r := send(t, http.MethodGet, "/trips/"+trip.ID.String(), nil, nil); r.Status != http.StatusOK || r.trip(t).ID != trip.ID {
		t.Errorf("existing: %d %s", r.Status, r.Body)
	}
	r := send(t, http.MethodGet, "/trips/"+uuid.NewString(), nil, nil)
	if r.Status != http.StatusNotFound || r.errorBody(t).Code != "TRIP_NOT_FOUND" {
		t.Errorf("missing: %d %s", r.Status, r.Body)
	}
	r = send(t, http.MethodGet, "/trips/not-a-uuid", nil, nil)
	if r.Status != http.StatusBadRequest || r.errorBody(t).Code != "VALIDATION_ERROR" {
		t.Errorf("invalid uuid: %d %s", r.Status, r.Body)
	}
}

func TestListTrips(t *testing.T) {
	setup(t)

	// Empty table: data must be [] (not null) and total_pages 0.
	r := send(t, http.MethodGet, "/trips", nil, nil)
	if r.Status != http.StatusOK || !strings.Contains(string(r.Body), `"data":[]`) {
		t.Fatalf("empty list: %d %s", r.Status, r.Body)
	}
	if p := r.list(t).Pagination; p.Page != 1 || p.PageSize != 20 || p.TotalItems != 0 || p.TotalPages != 0 {
		t.Errorf("empty pagination = %+v", p)
	}

	var created []domain.Trip
	for i := range 5 {
		created = append(created, mustCreate(t, fmt.Sprintf("T-LIST-%d", i)))
	}
	mustPatch(t, created[1].ID, domain.StatusAssigned)
	mustPatch(t, created[3].ID, domain.StatusAssigned)

	// Pages of 2, newest first, deterministic.
	var seen []domain.Trip
	for page := 1; page <= 3; page++ {
		lb := send(t, http.MethodGet, fmt.Sprintf("/trips?page=%d&page_size=2", page), nil, nil).list(t)
		if lb.Pagination.TotalItems != 5 || lb.Pagination.TotalPages != 3 || lb.Pagination.Page != page || lb.Pagination.PageSize != 2 {
			t.Errorf("page %d pagination = %+v", page, lb.Pagination)
		}
		seen = append(seen, lb.Data...)
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d trips, want 5", len(seen))
	}
	for i := range seen {
		if want := created[len(created)-1-i].ID; seen[i].ID != want {
			t.Errorf("position %d = %s, want %s (newest first)", i, seen[i].TripNumber, want)
		}
	}

	// Past the end: empty data, correct totals.
	lb := send(t, http.MethodGet, "/trips?page=4&page_size=2", nil, nil).list(t)
	if len(lb.Data) != 0 || lb.Pagination.TotalItems != 5 {
		t.Errorf("past end = %+v", lb)
	}

	// Status filter.
	lb = send(t, http.MethodGet, "/trips?status=ASSIGNED", nil, nil).list(t)
	if lb.Pagination.TotalItems != 2 || len(lb.Data) != 2 || lb.Data[0].ID != created[3].ID || lb.Data[1].ID != created[1].ID {
		t.Errorf("ASSIGNED filter = %+v", lb)
	}
	for _, trip := range lb.Data {
		if trip.Status != domain.StatusAssigned {
			t.Errorf("filter returned %s trip", trip.Status)
		}
	}
	if lb := send(t, http.MethodGet, "/trips?status=COMPLETED", nil, nil).list(t); lb.Pagination.TotalItems != 0 || len(lb.Data) != 0 {
		t.Errorf("COMPLETED filter = %+v", lb)
	}

	// Invalid parameters.
	for query, code := range map[string]string{
		"page=0":                  "VALIDATION_ERROR",
		"page_size=101":           "VALIDATION_ERROR",
		"page=x":                  "VALIDATION_ERROR",
		"status=done":             "INVALID_STATUS",
		"status=ASSIGNED'%20OR'1": "INVALID_STATUS",
	} {
		r := send(t, http.MethodGet, "/trips?"+query, nil, nil)
		if r.Status != http.StatusBadRequest || r.errorBody(t).Code != code {
			t.Errorf("%s: %d %s, want 400 %s", query, r.Status, r.Body, code)
		}
	}
}

func TestStatusLifecycle(t *testing.T) {
	setup(t)
	trip := mustCreate(t, "T-LIFE")

	prev := trip
	for _, next := range []domain.Status{domain.StatusAssigned, domain.StatusInTransit, domain.StatusCompleted} {
		got := mustPatch(t, trip.ID, next)
		if got.Status != next || got.UpdatedAt.Before(prev.UpdatedAt) || !got.CreatedAt.Equal(trip.CreatedAt) {
			t.Errorf("-> %s: got %+v (previous %+v)", next, got, prev)
		}
		prev = got
	}

	// COMPLETED is terminal.
	for _, next := range domain.AllStatuses {
		r := send(t, http.MethodPatch, "/trips/"+trip.ID.String()+"/status", map[string]string{"status": string(next)}, nil)
		if r.Status != http.StatusConflict || r.errorBody(t).Code != "INVALID_STATUS_TRANSITION" {
			t.Errorf("COMPLETED -> %s: %d %s", next, r.Status, r.Body)
		}
	}
	if s := dbStatus(t, trip.ID); s != domain.StatusCompleted {
		t.Errorf("db status = %s", s)
	}
}

func TestInvalidStatusUpdates(t *testing.T) {
	setup(t)
	trip := mustCreate(t, "T-BAD")
	path := "/trips/" + trip.ID.String() + "/status"

	cases := []struct {
		name       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"skip ASSIGNED", map[string]string{"status": "IN_TRANSIT"}, 409, "INVALID_STATUS_TRANSITION"},
		{"skip to COMPLETED", map[string]string{"status": "COMPLETED"}, 409, "INVALID_STATUS_TRANSITION"},
		{"same status", map[string]string{"status": "CREATED"}, 409, "INVALID_STATUS_TRANSITION"},
		{"unknown status", map[string]string{"status": "DONE"}, 400, "INVALID_STATUS"},
		{"lowercase status", map[string]string{"status": "assigned"}, 400, "INVALID_STATUS"},
		{"missing status", map[string]string{}, 400, "VALIDATION_ERROR"},
		{"extra field", map[string]string{"status": "ASSIGNED", "driver_name": "X"}, 400, "INVALID_JSON"},
		{"status not a string", `{"status": 1}`, 400, "INVALID_JSON"},
	}
	for _, c := range cases {
		r := send(t, http.MethodPatch, path, c.body, nil)
		if r.Status != c.wantStatus || r.errorBody(t).Code != c.wantCode {
			t.Errorf("%s: %d %s, want %d %s", c.name, r.Status, r.Body, c.wantStatus, c.wantCode)
		}
	}
	if s := dbStatus(t, trip.ID); s != domain.StatusCreated {
		t.Errorf("rejected updates changed status to %s", s)
	}

	r := send(t, http.MethodPatch, "/trips/"+uuid.NewString()+"/status", map[string]string{"status": "ASSIGNED"}, nil)
	if r.Status != http.StatusNotFound || r.errorBody(t).Code != "TRIP_NOT_FOUND" {
		t.Errorf("nonexistent trip: %d %s", r.Status, r.Body)
	}

	// Cancellation paths.
	a := mustCreate(t, "T-CANCEL-A")
	mustPatch(t, a.ID, domain.StatusCancelled)
	b := mustCreate(t, "T-CANCEL-B")
	mustPatch(t, b.ID, domain.StatusAssigned)
	mustPatch(t, b.ID, domain.StatusCancelled)
	c := mustCreate(t, "T-CANCEL-C")
	mustPatch(t, c.ID, domain.StatusAssigned)
	mustPatch(t, c.ID, domain.StatusInTransit)
	if r := send(t, http.MethodPatch, "/trips/"+c.ID.String()+"/status", map[string]string{"status": "CANCELLED"}, nil); r.Status != http.StatusConflict {
		t.Errorf("IN_TRANSIT -> CANCELLED: %d", r.Status)
	}
	if r := send(t, http.MethodPatch, "/trips/"+a.ID.String()+"/status", map[string]string{"status": "ASSIGNED"}, nil); r.Status != http.StatusConflict {
		t.Errorf("CANCELLED -> ASSIGNED: %d", r.Status)
	}
}

func TestDeleteTrip(t *testing.T) {
	setup(t)
	trip := mustCreate(t, "T-DEL")

	r := send(t, http.MethodDelete, "/trips/"+trip.ID.String(), nil, nil)
	if r.Status != http.StatusNoContent || len(r.Body) != 0 {
		t.Fatalf("delete: %d %s", r.Status, r.Body)
	}
	if r := send(t, http.MethodGet, "/trips/"+trip.ID.String(), nil, nil); r.Status != http.StatusNotFound {
		t.Errorf("get after delete: %d", r.Status)
	}
	// A second delete must not report success.
	r = send(t, http.MethodDelete, "/trips/"+trip.ID.String(), nil, nil)
	if r.Status != http.StatusNotFound || r.errorBody(t).Code != "TRIP_NOT_FOUND" {
		t.Errorf("second delete: %d %s", r.Status, r.Body)
	}
	if r := send(t, http.MethodDelete, "/trips/bad-id", nil, nil); r.Status != http.StatusBadRequest {
		t.Errorf("invalid uuid: %d", r.Status)
	}

	// Physical deletion frees the trip number and works for terminal trips.
	again := mustCreate(t, "T-DEL")
	mustPatch(t, again.ID, domain.StatusCancelled)
	if r := send(t, http.MethodDelete, "/trips/"+again.ID.String(), nil, nil); r.Status != http.StatusNoContent {
		t.Errorf("delete terminal trip: %d", r.Status)
	}
	if n := countRows(t, `SELECT count(*) FROM trips`); n != 0 {
		t.Errorf("rows after deletes = %d", n)
	}
}

func TestDatabaseConstraintsBackTheRules(t *testing.T) {
	setup(t)
	ctx := context.Background()
	// Even code that bypasses the API cannot store an invalid status or blank fields.
	_, err := pool.Exec(ctx, `INSERT INTO trips (id, trip_number, source, destination, driver_name, status)
		VALUES ($1, 'X', 'A', 'B', 'C', 'DONE')`, uuid.New())
	if err == nil || !strings.Contains(err.Error(), "trips_status_check") {
		t.Errorf("invalid status accepted by database: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO trips (id, trip_number, source, destination, driver_name)
		VALUES ($1, 'X', '  ', 'B', 'C')`, uuid.New())
	if err == nil || !strings.Contains(err.Error(), "trips_source_not_blank") {
		t.Errorf("blank source accepted by database: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO trips (id, trip_number, source, destination, driver_name)
		VALUES ($1, $2, 'A', 'B', 'C')`, uuid.New(), strings.Repeat("n", domain.MaxTripNumberLength+1))
	if err == nil {
		t.Error("over-long trip_number accepted by database")
	}
}
