package tests

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Tushar-programador/trip-management-api/internal/auth"
	"github.com/Tushar-programador/trip-management-api/internal/domain"
	"github.com/Tushar-programador/trip-management-api/internal/repository"
)

func TestTripEndpointsRequireAPIKey(t *testing.T) {
	setup(t)
	trip := mustCreate(t, "T-AUTH")
	unknownKey, err := auth.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/trips", tripInput("T-AUTH-NEW")},
		{http.MethodGet, "/trips", nil},
		{http.MethodGet, "/trips/" + trip.ID.String(), nil},
		{http.MethodPatch, "/trips/" + trip.ID.String() + "/status", map[string]string{"status": "ASSIGNED"}},
		{http.MethodDelete, "/trips/" + trip.ID.String(), nil},
	}
	credentials := map[string]string{
		"no key":       "",
		"wrong scheme": "Basic " + apiKey,
		"malformed":    "Bearer not-a-key",
		"unknown key":  "Bearer " + unknownKey,
	}
	for name, authorization := range credentials {
		for _, rt := range routes {
			r := send(t, rt.method, rt.path, rt.body, map[string]string{"Authorization": authorization})
			if r.Status != http.StatusUnauthorized || r.errorBody(t).Code != "UNAUTHENTICATED" {
				t.Errorf("%s %s (%s): %d %s, want 401 UNAUTHENTICATED", rt.method, rt.path, name, r.Status, r.Body)
			}
			if r.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s (%s): missing WWW-Authenticate", rt.method, rt.path, name)
			}
		}
	}

	// None of the rejected requests had any effect.
	if n := countRows(t, `SELECT count(*) FROM trips`); n != 1 {
		t.Errorf("trips = %d, want 1", n)
	}
	if s := dbStatus(t, trip.ID); s != domain.StatusCreated {
		t.Errorf("status = %s, want CREATED", s)
	}
}

func TestOperationalEndpointsArePublic(t *testing.T) {
	setup(t)
	for _, path := range []string{"/healthz", "/readyz", "/openapi.yaml"} {
		if r := send(t, http.MethodGet, path, nil, map[string]string{"Authorization": ""}); r.Status != http.StatusOK {
			t.Errorf("GET %s without a key: %d", path, r.Status)
		}
	}
}

func TestRevokedKeyIsRejectedImmediately(t *testing.T) {
	setup(t)
	ctx := context.Background()
	c, key, err := createClient(ctx, "to-be-revoked")
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + key}

	if r := send(t, http.MethodGet, "/trips", nil, headers); r.Status != http.StatusOK {
		t.Fatalf("before revocation: %d %s", r.Status, r.Body)
	}
	if _, err := repository.NewClientRepository(pool).Revoke(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if r := send(t, http.MethodGet, "/trips", nil, headers); r.Status != http.StatusUnauthorized {
		t.Errorf("after revocation: %d, want 401 on the very next request", r.Status)
	}
}

// TestIdempotencyKeysAreScopedPerClient: two clients using the same key never
// collide and never see each other's results.
func TestIdempotencyKeysAreScopedPerClient(t *testing.T) {
	setup(t)
	_, otherKey, err := createClient(context.Background(), "second-client")
	if err != nil {
		t.Fatal(err)
	}
	const shared = "shared-idempotency-key"
	asA := map[string]string{"Idempotency-Key": shared}
	asB := map[string]string{"Idempotency-Key": shared, "Authorization": "Bearer " + otherKey}

	a := send(t, http.MethodPost, "/trips", tripInput("T-SCOPE-A"), asA)
	b := send(t, http.MethodPost, "/trips", tripInput("T-SCOPE-B"), asB)
	mustStatus(t, a, http.StatusCreated)
	mustStatus(t, b, http.StatusCreated)
	if a.Header.Get("Idempotent-Replayed") != "" || b.Header.Get("Idempotent-Replayed") != "" {
		t.Fatal("first use of the key by each client must not be a replay")
	}
	tripA, tripB := a.trip(t), b.trip(t)
	if tripA.ID == tripB.ID {
		t.Fatal("two clients received the same trip")
	}

	// Each client's retry replays its own trip.
	if r := send(t, http.MethodPost, "/trips", tripInput("T-SCOPE-A"), asA); r.Header.Get("Idempotent-Replayed") != "true" || r.trip(t).ID != tripA.ID {
		t.Errorf("client A replay: %d %s", r.Status, r.Body)
	}
	if r := send(t, http.MethodPost, "/trips", tripInput("T-SCOPE-B"), asB); r.Header.Get("Idempotent-Replayed") != "true" || r.trip(t).ID != tripB.ID {
		t.Errorf("client B replay: %d %s", r.Status, r.Body)
	}
	// B cannot obtain A's trip by sending A's body under the shared key: for B
	// that key is bound to B's own (different) request.
	if r := send(t, http.MethodPost, "/trips", tripInput("T-SCOPE-A"), asB); r.Status != http.StatusConflict || r.errorBody(t).Code != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("cross-client body under shared key: %d %s", r.Status, r.Body)
	}

	if n := countRows(t, `SELECT count(*) FROM trips`); n != 2 {
		t.Errorf("trips = %d, want 2", n)
	}
	if n := countRows(t, `SELECT count(*) FROM idempotency_keys`); n != 2 {
		t.Errorf("idempotency keys = %d, want one per client", n)
	}
}

func TestClientAdministration(t *testing.T) {
	setup(t)
	ctx := context.Background()
	clients := repository.NewClientRepository(pool)

	c, key, err := createClient(ctx, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	if c.KeyPrefix != auth.DisplayPrefix(key) || c.RevokedAt != nil || c.CreatedAt.IsZero() {
		t.Errorf("created client = %+v", c)
	}
	// Only the hash is stored, never the key.
	if n := countRows(t, `SELECT count(*) FROM api_clients WHERE key_hash = $1`, auth.HashKey(key)); n != 1 {
		t.Errorf("rows with key hash = %d", n)
	}
	if n := countRows(t, `SELECT count(*) FROM api_clients WHERE key_hash = $1 OR key_prefix = $1`, key); n != 0 {
		t.Error("plaintext key found in api_clients")
	}

	all, err := clients.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range all {
		found = found || x.ID == c.ID
	}
	if !found {
		t.Error("created client missing from List")
	}

	first, err := clients.Revoke(ctx, c.ID)
	if err != nil || first.RevokedAt == nil {
		t.Fatalf("revoke: %+v %v", first, err)
	}
	again, err := clients.Revoke(ctx, c.ID)
	if err != nil || !again.RevokedAt.Equal(*first.RevokedAt) {
		t.Errorf("second revoke must keep the original time: %v vs %v (%v)", again.RevokedAt, first.RevokedAt, err)
	}
	if _, err := clients.Revoke(ctx, uuid.New()); !errors.Is(err, domain.ErrClientNotFound) {
		t.Errorf("revoke unknown client: %v", err)
	}
}

// TestAPIRoleCannotManageAPIKeys: the API can look clients up but cannot
// create, re-enable or delete them, so a compromised API process cannot mint
// itself credentials or undo a revocation.
func TestAPIRoleCannotManageAPIKeys(t *testing.T) {
	setup(t)
	if appPool == nil {
		t.Skip("TEST_APP_DATABASE_URL not set")
	}
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO api_clients (id, name, key_prefix, key_hash) VALUES (gen_random_uuid(), 'x', 'tma_x', repeat('0', 64))`,
		`UPDATE api_clients SET revoked_at = NULL`,
		`DELETE FROM api_clients`,
	} {
		_, err := appPool.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: want permission denied (42501), got %v", stmt, err)
		}
	}
}
