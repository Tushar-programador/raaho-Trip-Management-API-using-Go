// Package service contains the trip use cases. It applies input rules and
// orchestrates the repository; it knows nothing about HTTP.
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
	"github.com/Tushar-programador/trip-management-api/internal/repository"
)

// TripService implements the trip use cases.
type TripService struct {
	repo           *repository.TripRepository
	idempotencyTTL time.Duration
}

// NewTripService returns a TripService. idempotencyTTL is how long an
// Idempotency-Key is remembered after first use.
func NewTripService(repo *repository.TripRepository, idempotencyTTL time.Duration) *TripService {
	return &TripService{repo: repo, idempotencyTTL: idempotencyTTL}
}

// CreateResult is a created (or replayed) trip.
type CreateResult struct {
	Trip     domain.Trip
	Replayed bool // true when an earlier result was returned for the same Idempotency-Key
}

// Create validates the input and stores a new trip in status CREATED on
// behalf of clientID. idempotencyKey is optional; pass "" when the client did
// not send one.
func (s *TripService) Create(ctx context.Context, clientID uuid.UUID, in domain.CreateTripInput, idempotencyKey string) (CreateResult, error) {
	in = in.Normalize()
	if err := in.Validate(); err != nil {
		return CreateResult{}, err
	}

	var key *domain.IdempotencyKey
	if idempotencyKey != "" {
		if err := domain.ValidateIdempotencyKey(idempotencyKey); err != nil {
			return CreateResult{}, err
		}
		key = &domain.IdempotencyKey{
			ClientID:    clientID,
			Scope:       domain.IdempotencyScopeCreateTrip,
			Key:         idempotencyKey,
			RequestHash: in.Fingerprint(),
			TTL:         s.idempotencyTTL,
		}
	}

	// UUIDv7 is time-ordered, which keeps primary-key index inserts local.
	id, err := uuid.NewV7()
	if err != nil {
		return CreateResult{}, fmt.Errorf("generate trip id: %w", err)
	}
	trip := domain.Trip{
		ID:          id,
		TripNumber:  in.TripNumber,
		Source:      in.Source,
		Destination: in.Destination,
		DriverName:  in.DriverName,
		Status:      domain.StatusCreated,
	}

	created, replayed, err := s.repo.Create(ctx, trip, key)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Trip: created, Replayed: replayed}, nil
}

// Get returns one trip.
func (s *TripService) Get(ctx context.Context, id uuid.UUID) (domain.Trip, error) {
	return s.repo.GetByID(ctx, id)
}

// List returns a filtered page of trips.
func (s *TripService) List(ctx context.Context, f domain.ListFilter) (domain.TripPage, error) {
	return s.repo.List(ctx, f)
}

// UpdateStatus moves a trip to a new status if the state machine allows it.
// The check runs inside the repository's locking transaction, against the
// persisted status, never against a value read earlier.
func (s *TripService) UpdateStatus(ctx context.Context, id uuid.UUID, rawStatus string) (domain.Trip, error) {
	if rawStatus == "" {
		return domain.Trip{}, &domain.ValidationError{Fields: []domain.FieldError{{Field: "status", Message: "is required"}}}
	}
	to, ok := domain.ParseStatus(rawStatus)
	if !ok {
		return domain.Trip{}, &domain.InvalidStatusError{Value: rawStatus}
	}
	return s.repo.UpdateStatus(ctx, id, to)
}

// Delete physically deletes a trip in any status.
func (s *TripService) Delete(ctx context.Context, id uuid.UUID) error {
	return s.repo.Delete(ctx, id)
}

// Ready reports whether the service can reach its database.
func (s *TripService) Ready(ctx context.Context) error {
	return s.repo.Ping(ctx)
}
