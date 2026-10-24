package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
)

// race runs n functions so they start as close to simultaneously as possible:
// every goroutine blocks on a shared channel that is closed once all are
// ready. No sleeps are involved.
func race(n int, fn func(i int)) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := range n {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			fn(i)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
}

type patchResult struct {
	target domain.Status
	status int
	body   []byte
}

func patchConcurrently(t *testing.T, id fmt.Stringer, targets []domain.Status) []patchResult {
	results := make([]patchResult, len(targets))
	race(len(targets), func(i int) {
		r := send(t, http.MethodPatch, "/trips/"+id.String()+"/status", map[string]string{"status": string(targets[i])}, nil)
		results[i] = patchResult{target: targets[i], status: r.Status, body: r.Body}
	})
	return results
}

// TestConcurrentIdenticalTransitions: many requests race to apply the same
// valid transition. With a check-then-write on stale state several would
// "succeed"; with the row lock exactly one does and the rest see ASSIGNED.
// Several rounds make a lost race very unlikely to go unnoticed.
func TestConcurrentIdenticalTransitions(t *testing.T) {
	setup(t)
	targets := make([]domain.Status, 25)
	for i := range targets {
		targets[i] = domain.StatusAssigned
	}
	for round := range 15 {
		trip := mustCreate(t, fmt.Sprintf("T-CONC-SAME-%d", round))
		var ok, conflict int
		for _, r := range patchConcurrently(t, trip.ID, targets) {
			switch r.status {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflict++
			default:
				t.Errorf("round %d: unexpected response %d: %s", round, r.status, r.body)
			}
		}
		if ok != 1 || conflict != len(targets)-1 {
			t.Errorf("round %d: ok=%d conflict=%d, want exactly 1 success", round, ok, conflict)
		}
		if s := dbStatus(t, trip.ID); s != domain.StatusAssigned {
			t.Errorf("round %d: final status = %s", round, s)
		}
	}
}

// validPaths lists every set of statuses that can be reached from CREATED,
// keyed by the status the trip ends in.
var validPaths = map[domain.Status][]domain.Status{
	domain.StatusCreated:   {},
	domain.StatusAssigned:  {domain.StatusAssigned},
	domain.StatusInTransit: {domain.StatusAssigned, domain.StatusInTransit},
	domain.StatusCompleted: {domain.StatusAssigned, domain.StatusInTransit, domain.StatusCompleted},
	domain.StatusCancelled: nil, // CREATED->CANCELLED or CREATED->ASSIGNED->CANCELLED; checked separately
}

// TestConcurrentCompetingTransitions fires a mix of all transitions at each
// trip simultaneously and checks the invariants that must hold however the
// requests interleave:
//
//   - the state machine is acyclic, so each status can be entered at most once;
//   - the successful transitions form a valid path from CREATED;
//   - the persisted status is the end of that path;
//   - every rejection is a 409 INVALID_STATUS_TRANSITION, never a 500.
func TestConcurrentCompetingTransitions(t *testing.T) {
	setup(t)
	targets := []domain.Status{
		domain.StatusAssigned, domain.StatusInTransit, domain.StatusCompleted, domain.StatusCancelled,
		domain.StatusAssigned, domain.StatusInTransit, domain.StatusCompleted, domain.StatusCancelled,
		domain.StatusAssigned, domain.StatusInTransit, domain.StatusCompleted, domain.StatusCancelled,
		domain.StatusAssigned, domain.StatusInTransit, domain.StatusCompleted, domain.StatusCancelled,
	}
	for round := range 20 {
		trip := mustCreate(t, fmt.Sprintf("T-CONC-MIX-%d", round))

		var succeeded []domain.Status
		for _, r := range patchConcurrently(t, trip.ID, targets) {
			switch r.status {
			case http.StatusOK:
				var got domain.Trip
				if err := json.Unmarshal(r.body, &got); err != nil || got.Status != r.target {
					t.Errorf("round %d: 200 for %s returned %s", round, r.target, r.body)
				}
				succeeded = append(succeeded, r.target)
			case http.StatusConflict:
				var env struct{ Error struct{ Code string } }
				if json.Unmarshal(r.body, &env) != nil || env.Error.Code != "INVALID_STATUS_TRANSITION" {
					t.Errorf("round %d: unexpected 409 body %s", round, r.body)
				}
			default:
				t.Errorf("round %d: unexpected response %d: %s", round, r.status, r.body)
			}
		}

		final := dbStatus(t, trip.ID)
		slices.Sort(succeeded)
		if !isValidPath(final, succeeded) {
			t.Errorf("round %d: final status %s with successful transitions %v is not a valid lifecycle", round, final, succeeded)
		}
	}
}

func isValidPath(final domain.Status, succeeded []domain.Status) bool {
	if final == domain.StatusCancelled {
		a := []domain.Status{domain.StatusCancelled}
		b := []domain.Status{domain.StatusAssigned, domain.StatusCancelled}
		slices.Sort(a)
		slices.Sort(b)
		return slices.Equal(succeeded, a) || slices.Equal(succeeded, b)
	}
	want := slices.Clone(validPaths[final])
	slices.Sort(want)
	return slices.Equal(succeeded, want)
}

// TestConcurrentAssignVersusInTransit is the scenario from the requirements:
// CREATED->ASSIGNED and ->IN_TRANSIT race. IN_TRANSIT may only succeed if it
// is applied after ASSIGNED committed; it must never be accepted from CREATED.
// (Both outcomes are legal, so this checks the result space; the lock itself
// is proven deterministically by TestStatusUpdateWaitsForRowLock.)
func TestConcurrentAssignVersusInTransit(t *testing.T) {
	setup(t)
	for round := range 30 {
		trip := mustCreate(t, fmt.Sprintf("T-CONC-AIT-%d", round))
		res := patchConcurrently(t, trip.ID, []domain.Status{domain.StatusAssigned, domain.StatusInTransit})
		assign, transit := res[0].status, res[1].status
		final := dbStatus(t, trip.ID)

		switch {
		case assign == 200 && transit == 200 && final == domain.StatusInTransit:
		case assign == 200 && transit == 409 && final == domain.StatusAssigned:
		default:
			t.Errorf("round %d: assign=%d in_transit=%d final=%s", round, assign, transit, final)
		}
	}
}

// TestStatusUpdateWaitsForRowLock proves deterministically that the update
// validates against committed state, not a stale read: another transaction
// locks the row and changes the status without committing; the API request
// must block on the lock (observed via pg_stat_activity, not a sleep) and,
// after the commit, validate against the *new* status.
func TestStatusUpdateWaitsForRowLock(t *testing.T) {
	setup(t)
	ctx := context.Background()

	cases := []struct {
		name        string
		committed   domain.Status // what the competing transaction writes
		request     domain.Status // what the API request asks for
		wantHTTP    int
		wantFinal   domain.Status
		explanation string
	}{
		{"sees new state and succeeds", domain.StatusAssigned, domain.StatusInTransit, 200, domain.StatusInTransit,
			"from the stale CREATED it would have been rejected"},
		{"sees new state and is rejected", domain.StatusCancelled, domain.StatusAssigned, 409, domain.StatusCancelled,
			"from the stale CREATED it would have been accepted, resurrecting a cancelled trip"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trip := mustCreate(t, fmt.Sprintf("T-LOCK-%d", i))

			tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SELECT 1 FROM trips WHERE id = $1 FOR UPDATE`, trip.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `UPDATE trips SET status = $2, updated_at = now() WHERE id = $1`, trip.ID, c.committed); err != nil {
				t.Fatal(err)
			}

			done := make(chan response, 1)
			go func() {
				done <- send(t, http.MethodPatch, "/trips/"+trip.ID.String()+"/status", map[string]string{"status": string(c.request)}, nil)
			}()

			waitForLockWaiter(t)
			select {
			case r := <-done:
				t.Fatalf("request finished while the row was locked: %d %s", r.Status, r.Body)
			default:
			}

			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			r := <-done
			if r.Status != c.wantHTTP {
				t.Errorf("status %d (%s), want %d: %s", r.Status, r.Body, c.wantHTTP, c.explanation)
			}
			if s := dbStatus(t, trip.ID); s != c.wantFinal {
				t.Errorf("final status %s, want %s", s, c.wantFinal)
			}
		})
	}
}

// TestStatusUpdateLockTimeout: when a row stays locked longer than
// lock_timeout, PostgreSQL itself abandons the wait. The client gets a
// retryable 503, the trip is untouched, and no backend is left queued on the
// lock (a cancelled Go context alone would leave the server-side waiter behind).
func TestStatusUpdateLockTimeout(t *testing.T) {
	setup(t)
	ctx := context.Background()
	trip := mustCreate(t, "T-LOCK-TIMEOUT")

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM trips WHERE id = $1 FOR UPDATE`, trip.ID); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	r := send(t, http.MethodPatch, "/trips/"+trip.ID.String()+"/status", map[string]string{"status": "ASSIGNED"}, nil)
	if r.Status != http.StatusServiceUnavailable || r.errorBody(t).Code != "SERVICE_UNAVAILABLE" {
		t.Fatalf("status %d: %s, want 503 SERVICE_UNAVAILABLE", r.Status, r.Body)
	}
	if elapsed := time.Since(start); elapsed < lockTimeout {
		t.Errorf("returned after %s, before lock_timeout %s", elapsed, lockTimeout)
	}
	if n := countRows(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`); n != 0 {
		t.Errorf("%d backends still waiting on a lock", n)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if s := dbStatus(t, trip.ID); s != domain.StatusCreated {
		t.Errorf("status = %s after timed-out update", s)
	}
}

// waitForLockWaiter polls PostgreSQL until a backend is blocked on a lock.
func waitForLockWaiter(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if countRows(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond) // polling interval only; correctness does not depend on it
	}
	t.Fatal("API request never blocked on the row lock")
}

// TestConcurrentDuplicateTripNumbers: the UNIQUE constraint, not an
// application pre-check, decides the winner.
func TestConcurrentDuplicateTripNumbers(t *testing.T) {
	setup(t)
	const n = 25
	statuses := make([]int, n)
	race(n, func(i int) {
		statuses[i] = send(t, http.MethodPost, "/trips", tripInput("T-CONC-DUP"), nil).Status
	})

	var created, conflict int
	for _, s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	if created != 1 || conflict != n-1 {
		t.Errorf("created=%d conflict=%d", created, conflict)
	}
	if rows := countRows(t, `SELECT count(*) FROM trips`); rows != 1 {
		t.Errorf("rows = %d, want 1", rows)
	}
}
