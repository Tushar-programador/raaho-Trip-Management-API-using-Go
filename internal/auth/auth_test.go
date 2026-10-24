package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
)

func TestNewKey(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		key, err := NewKey()
		if err != nil {
			t.Fatal(err)
		}
		if len(key) != KeyLength || !strings.HasPrefix(key, "tma_") || !WellFormed(key) {
			t.Fatalf("malformed key %q", key)
		}
		if seen[key] {
			t.Fatal("duplicate key generated")
		}
		seen[key] = true
	}
}

func TestHashKeyAndPrefix(t *testing.T) {
	key, _ := NewKey()
	other, _ := NewKey()
	if HashKey(key) != HashKey(key) || len(HashKey(key)) != 64 {
		t.Errorf("hash must be a deterministic 64-char hex digest: %q", HashKey(key))
	}
	if HashKey(key) == HashKey(other) {
		t.Error("different keys produced the same hash")
	}
	if p := DisplayPrefix(key); len(p) != 12 || !strings.HasPrefix(key, p) {
		t.Errorf("DisplayPrefix = %q", p)
	}
}

func TestWellFormed(t *testing.T) {
	valid, _ := NewKey()
	for _, s := range []string{
		"",
		"tma_",
		valid[:len(valid)-1],          // too short
		valid + "A",                   // too long
		"xyz_" + valid[4:],            // wrong scheme
		valid[:10] + "!" + valid[11:], // character outside base64url
		valid[:10] + " " + valid[11:], // space
		strings.ToUpper(valid[:4]) + valid[4:],
	} {
		if WellFormed(s) {
			t.Errorf("WellFormed(%q) = true", s)
		}
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]struct {
		token string
		ok    bool
	}{
		"Bearer abc":   {"abc", true},
		"bearer abc":   {"abc", true},
		"BEARER  abc ": {"abc", true},
		"Basic abc":    {"", false},
		"Bearer":       {"", false},
		"Bearer ":      {"", false},
		"abc":          {"", false},
		"":             {"", false},
	}
	for header, want := range cases {
		token, ok := bearerToken(header)
		if token != want.token || ok != want.ok {
			t.Errorf("bearerToken(%q) = %q, %v; want %q, %v", header, token, ok, want.token, want.ok)
		}
	}
}

type fakeStore struct {
	keyHash string
	client  domain.Client
	err     error
	calls   int
}

func (s *fakeStore) FindActiveByKeyHash(_ context.Context, keyHash string) (domain.Client, error) {
	s.calls++
	if s.err != nil {
		return domain.Client{}, s.err
	}
	if keyHash != s.keyHash {
		return domain.Client{}, domain.ErrUnauthenticated
	}
	return s.client, nil
}

func TestMiddleware(t *testing.T) {
	key, _ := NewKey()
	unknown, _ := NewKey()
	client := domain.Client{ID: uuid.New(), Name: "test-client"}

	cases := []struct {
		name       string
		header     string
		storeErr   error
		wantErr    error // nil means the request reaches the handler
		wantLookup bool
	}{
		{"valid key", "Bearer " + key, nil, nil, true},
		{"no header", "", nil, domain.ErrUnauthenticated, false},
		{"wrong scheme", "Basic " + key, nil, domain.ErrUnauthenticated, false},
		{"malformed key", "Bearer not-a-key", nil, domain.ErrUnauthenticated, false},
		{"unknown or revoked key", "Bearer " + unknown, nil, domain.ErrUnauthenticated, true},
		{"database unavailable", "Bearer " + key, domain.ErrUnavailable, domain.ErrUnavailable, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{keyHash: HashKey(key), client: client, err: c.storeErr}
			var gotErr error
			var reached bool
			h := Middleware(store, func(w http.ResponseWriter, _ *http.Request, err error) {
				gotErr = err
				w.WriteHeader(http.StatusUnauthorized)
			})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				got, ok := ClientFromContext(r.Context())
				if !ok || got.ID != client.ID {
					t.Errorf("client in context = %+v, %v", got, ok)
				}
			}))

			req := httptest.NewRequest(http.MethodGet, "/trips", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if c.wantErr == nil && (!reached || gotErr != nil) {
				t.Errorf("valid request rejected: %v", gotErr)
			}
			if c.wantErr != nil && (reached || !errors.Is(gotErr, c.wantErr)) {
				t.Errorf("reached=%v err=%v, want rejection with %v", reached, gotErr, c.wantErr)
			}
			if (store.calls > 0) != c.wantLookup {
				t.Errorf("store lookups = %d, want lookup %v", store.calls, c.wantLookup)
			}
		})
	}
}
