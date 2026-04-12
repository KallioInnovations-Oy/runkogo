package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ==========================================================================
// MemoryStore — in-memory implementation
// ==========================================================================
// Use this for development, testing, and the scaffold demo.
// Data lives only for the lifetime of the process.
//
// Thread-safe via sync.RWMutex. Suitable for single-instance use.
// For multi-instance deployments, switch to PostgresStore.

type MemoryStore struct {
	mu    sync.RWMutex
	users map[string]User
	seq   int
}

// NewMemoryStore creates a store with optional seed data.
func NewMemoryStore(seed bool) *MemoryStore {
	s := &MemoryStore{
		users: make(map[string]User),
		seq:   0,
	}
	if seed {
		s.users["1"] = User{
			ID:        "1",
			Name:      "Demo User",
			Email:     "demo@example.com",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		s.seq = 1
	}
	return s
}

func (s *MemoryStore) List(ctx context.Context) ([]User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}

	// Sort by created_at descending (newest first).
	sort.Slice(users, func(i, j int) bool {
		return users[i].CreatedAt.After(users[j].CreatedAt)
	})

	return users, nil
}

func (s *MemoryStore) GetByID(ctx context.Context, id string) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	u, ok := s.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (s *MemoryStore) Create(ctx context.Context, name, email string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check email uniqueness.
	for _, u := range s.users {
		if u.Email == email {
			return User{}, ErrConflict
		}
	}

	s.seq++
	now := time.Now()
	u := User{
		ID:        fmt.Sprintf("%d", s.seq),
		Name:      name,
		Email:     email,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.users[u.ID] = u
	return u, nil
}

func (s *MemoryStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.users[id]; !ok {
		return ErrNotFound
	}
	delete(s.users, id)
	return nil
}

func (s *MemoryStore) Ping(ctx context.Context) error {
	// Memory is always available.
	return nil
}

func (s *MemoryStore) Close() error {
	// Nothing to close.
	return nil
}
