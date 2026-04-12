package main

import (
	"context"
	"errors"
	"time"
)

// ==========================================================================
// Store interface — the contract
// ==========================================================================
// This is the boundary between your business logic and your data layer.
// Handlers depend on this interface, never on a concrete implementation.
// Swap between MemoryStore (dev/test) and PostgresStore (production)
// with a single line change in main().
//
// Design rules:
//   - Every method takes context.Context (for timeouts, cancellation).
//   - Errors are returned, never panicked.
//   - The interface is domain-specific (UserStore), not generic (Store).
//     This keeps method signatures clear and avoids interface pollution.

// UserStore defines all operations on users.
type UserStore interface {
	// List returns all users, ordered by creation time descending.
	List(ctx context.Context) ([]User, error)

	// GetByID returns a single user. Returns ErrNotFound if not found.
	GetByID(ctx context.Context, id string) (User, error)

	// Create inserts a new user and returns it with ID and timestamps set.
	Create(ctx context.Context, name, email string) (User, error)

	// Delete removes a user by ID. Returns ErrNotFound if not found.
	Delete(ctx context.Context, id string) error

	// Ping checks the data store connectivity. Used by health checks.
	Ping(ctx context.Context) error

	// Close releases any held resources (connections, pools).
	// Called during graceful shutdown.
	Close() error
}

// ==========================================================================
// Domain types
// ==========================================================================

// User represents a user in the system. This type is shared across
// all store implementations and the HTTP handlers.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ==========================================================================
// Sentinel errors
// ==========================================================================
// Use errors.Is(err, ErrNotFound) in handlers to distinguish
// "not found" from actual data errors.

var (
	// ErrNotFound is returned when a requested entity does not exist.
	ErrNotFound = errors.New("not found")

	// ErrConflict is returned when a create/update violates a uniqueness
	// constraint (e.g., duplicate email).
	ErrConflict = errors.New("conflict")
)
