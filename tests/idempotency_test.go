package tests

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestIdempotentCreateReplaysOriginal(t *testing.T) {
	setup(t)
	key := map[string]string{"Idempotency-Key": uuid.NewString()}

	first := send(t, http.MethodPost, "/trips", tripInput("T-IDEM-1"), key)
	if first.Status != http.StatusCreated || first.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("first: %d %v %s", first.Status, first.Header, first.Body)
	}
	original := first.trip(t)

	// Change the trip afterwards: the replay must still return the original response.
	mustPatch(t, original.ID, "ASSIGNED")

	// Same key, semantically identical body (different whitespace and key order).
	body := `{"driver_name":"Ravi Kumar","destination":"Pune","source":" Mumbai ","trip_number":"T-IDEM-1"}`
	replay := send(t, http.MethodPost, "/trips", body, key)
	if replay.Status != http.StatusCreated || replay.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v %s", replay.Status, replay.Header, replay.Body)
	}
	if got := replay.trip(t); got != original {
		t.Errorf("replay = %+v, want original %+v", got, original)
	}
	if n := countRows(t, `SELECT count(*) FROM trips`); n != 1 {
		t.Errorf("trips = %d, want 1", n)
	}

	// Same key, different body.
	other := tripInput("T-IDEM-2")
	r := send(t, http.MethodPost, "/trips", other, key)
	if r.Status != http.StatusConflict || r.errorBody(t).Code != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("reused key: %d %s", r.Status, r.Body)
	}
	if n := countRows(t, `SELECT count(*) FROM trips`); n != 1 {
		t.Errorf("trips = %d after key reuse, want 1", n)
	}
}

func TestIdempotencyKeyNotConsumedByFailedRequest(t *testing.T) {
	setup(t)
	mustCreate(t, "T-TAKEN")
	key := map[string]string{"Idempotency-Key": "retry-after-fix"}

	if r := send(t, http.MethodPost, "/trips", tripInput("T-TAKEN"), key); r.Status != http.StatusConflict || r.errorBody(t).Code != "TRIP_NUMBER_EXISTS" {
		t.Fatalf("expected duplicate: %d %s", r.Status, r.Body)
	}
	// The failed attempt rolled back with its key, so the client can fix the
	// request and reuse the key.
	if r := send(t, http.MethodPost, "/trips", tripInput("T-FREE"), key); r.Status != http.StatusCreated {
		t.Errorf("retry with corrected body: %d %s", r.Status, r.Body)
	}
}

func TestExpiredIdempotencyKeyIsReclaimed(t *testing.T) {
	setup(t)
	key := map[string]string{"Idempotency-Key": "expiring-key"}
	mustStatus(t, send(t, http.MethodPost, "/trips", tripInput("T-EXP-1"), key), http.StatusCreated)

	if _, err := pool.Exec(context.Background(),
		`UPDATE idempotency_keys SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	r := send(t, http.MethodPost, "/trips", tripInput("T-EXP-2"), key)
	mustStatus(t, r, http.StatusCreated)
	if r.Header.Get("Idempotent-Replayed") != "" {
		t.Error("expired key must not replay")
	}
	if n := countRows(t, `SELECT count(*) FROM trips`); n != 2 {
		t.Errorf("trips = %d, want 2", n)
	}
}

func TestInvalidIdempotencyKey(t *testing.T) {
	setup(t)
	r := send(t, http.MethodPost, "/trips", tripInput("T-BADKEY"), map[string]string{"Idempotency-Key": "has spaces in it"})
	if r.Status != http.StatusBadRequest || r.errorBody(t).Code != "VALIDATION_ERROR" {
		t.Errorf("%d %s", r.Status, r.Body)
	}
}

// TestConcurrentIdempotentCreates: simultaneous requests with one key and one
// body create exactly one trip and all receive it.
func TestConcurrentIdempotentCreates(t *testing.T) {
	setup(t)
	const n = 25
	key := map[string]string{"Idempotency-Key": uuid.NewString()}
	results := make([]response, n)
	race(n, func(i int) {
		results[i] = send(t, http.MethodPost, "/trips", tripInput("T-IDEM-CONC"), key)
	})

	var ids = map[uuid.UUID]int{}
	replays := 0
	for _, r := range results {
		if r.Status != http.StatusCreated {
			t.Errorf("status %d: %s", r.Status, r.Body)
			continue
		}
		ids[r.trip(t).ID]++
		if r.Header.Get("Idempotent-Replayed") == "true" {
			replays++
		}
	}
	if len(ids) != 1 || replays != n-1 {
		t.Errorf("distinct trip ids = %d, replays = %d; want 1 and %d", len(ids), replays, n-1)
	}
	if rows := countRows(t, `SELECT count(*) FROM trips`); rows != 1 {
		t.Errorf("trips = %d, want 1", rows)
	}
	if rows := countRows(t, `SELECT count(*) FROM idempotency_keys`); rows != 1 {
		t.Errorf("idempotency keys = %d, want 1", rows)
	}
}

// TestConcurrentIdempotencyKeyConflict: one key, two different bodies, sent
// simultaneously. Exactly one body wins; the other is rejected with 409, and
// never creates a second trip.
func TestConcurrentIdempotencyKeyConflict(t *testing.T) {
	setup(t)
	const n = 20
	key := map[string]string{"Idempotency-Key": uuid.NewString()}
	results := make([]response, n)
	race(n, func(i int) {
		number := "T-IDEM-A"
		if i%2 == 1 {
			number = "T-IDEM-B"
		}
		results[i] = send(t, http.MethodPost, "/trips", tripInput(number), key)
	})

	winners := map[string]bool{}
	for _, r := range results {
		switch r.Status {
		case http.StatusCreated:
			winners[r.trip(t).TripNumber] = true
		case http.StatusConflict:
			if code := r.errorBody(t).Code; code != "IDEMPOTENCY_KEY_REUSED" {
				t.Errorf("409 code = %s", code)
			}
		default:
			t.Errorf("status %d: %s", r.Status, r.Body)
		}
	}
	if len(winners) != 1 {
		t.Errorf("winning bodies = %v, want exactly one", winners)
	}
	if rows := countRows(t, `SELECT count(*) FROM trips`); rows != 1 {
		t.Errorf("trips = %d, want 1", rows)
	}
}

func mustStatus(t *testing.T, r response, want int) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status %d, want %d: %s", r.Status, want, r.Body)
	}
}
