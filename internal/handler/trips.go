// Package handler is the HTTP transport: routing, request parsing, response
// serialization and error-to-status mapping.
package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Tushar-programador/trip-management-api/internal/auth"
	"github.com/Tushar-programador/trip-management-api/internal/domain"
	"github.com/Tushar-programador/trip-management-api/internal/service"
)

// IdempotencyKeyHeader is the optional header that makes POST /trips safe to retry.
const IdempotencyKeyHeader = "Idempotency-Key"

// TripService is the behaviour the handlers need. It is declared here, at the
// consumer, so handler tests can supply a fake without a database.
type TripService interface {
	Create(ctx context.Context, clientID uuid.UUID, in domain.CreateTripInput, idempotencyKey string) (service.CreateResult, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Trip, error)
	List(ctx context.Context, f domain.ListFilter) (domain.TripPage, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status string) (domain.Trip, error)
	Delete(ctx context.Context, id uuid.UUID) error
	Ready(ctx context.Context) error
}

// TripHandler serves the /trips resource.
type TripHandler struct {
	svc TripService
	log *slog.Logger
}

type listResponse struct {
	Data       []domain.Trip `json:"data"`
	Pagination Pagination    `json:"pagination"`
}

type updateStatusRequest struct {
	Status string `json:"status"`
}

func (h *TripHandler) create(w http.ResponseWriter, r *http.Request) {
	var in domain.CreateTripInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, r, h.log, err)
		return
	}
	// A header that is present but empty is a client bug, not "no key".
	key := r.Header.Get(IdempotencyKeyHeader)
	if _, present := r.Header[IdempotencyKeyHeader]; present {
		if err := domain.ValidateIdempotencyKey(key); err != nil {
			writeError(w, r, h.log, err)
			return
		}
	}

	// The /trips routes are behind auth.Middleware, so a client is present.
	client, ok := auth.ClientFromContext(r.Context())
	if !ok {
		writeError(w, r, h.log, domain.ErrUnauthenticated)
		return
	}
	res, err := h.svc.Create(r.Context(), client.ID, in, key)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	w.Header().Set("Location", "/trips/"+res.Trip.ID.String())
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusCreated, res.Trip)
}

func (h *TripHandler) list(w http.ResponseWriter, r *http.Request) {
	f, err := parseListFilter(r.URL.Query())
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	page, err := h.svc.List(r.Context(), f)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	data := page.Trips
	if data == nil {
		data = []domain.Trip{} // always encode "data": [], never null
	}
	writeJSON(w, http.StatusOK, listResponse{Data: data, Pagination: newPagination(f, page.TotalItems)})
}

func (h *TripHandler) get(w http.ResponseWriter, r *http.Request) {
	id, err := tripID(r)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	trip, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, trip)
}

func (h *TripHandler) updateStatus(w http.ResponseWriter, r *http.Request) {
	id, err := tripID(r)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	var req updateStatusRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, h.log, err)
		return
	}
	trip, err := h.svc.UpdateStatus(r.Context(), id, req.Status)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, trip)
}

func (h *TripHandler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := tripID(r)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		writeError(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tripID parses the {id} path parameter. Only the canonical 36-character
// hyphenated form is accepted (uuid.Parse alone also allows URN, braced and
// unhyphenated forms, which would give one trip several URLs).
func tripID(r *http.Request) (uuid.UUID, error) {
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil || len(raw) != 36 {
		return uuid.Nil, &domain.ValidationError{Fields: []domain.FieldError{{Field: "id", Message: "must be a valid UUID"}}}
	}
	return id, nil
}
