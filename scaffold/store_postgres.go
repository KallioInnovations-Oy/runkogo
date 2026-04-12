package main

// ==========================================================================
// PostgresStore — production database implementation (STUB)
// ==========================================================================
//
// This file shows the complete structure of a PostgreSQL-backed store.
// To activate it:
//
//   1. Add the driver: go get github.com/jackc/pgx/v5
//   2. Uncomment the code below.
//   3. In main(), swap:  store := NewMemoryStore(true)
//                   to:  store := NewPostgresStore(ctx, dbURL)
//
// The rest of the application doesn't change — handlers depend on the
// UserStore interface, not the implementation.
//
// Connection string from environment:
//   DATABASE_URL=postgres://user:pass@localhost:5432/runko?sslmode=disable
//
// ==========================================================================

/*
import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore implements UserStore backed by PostgreSQL.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore connects to PostgreSQL and returns a ready store.
// Call Close() during shutdown to release the connection pool.
func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}

	// Connection pool tuning.
	config.MaxConns = 20
	config.MinConns = 2
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	// Verify connectivity.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database ping failed: %w", err)
	}

	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) List(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, email, created_at, updated_at
		 FROM users
		 ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *PostgresStore) GetByID(ctx context.Context, id string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, email, created_at, updated_at
		 FROM users
		 WHERE id = $1`, id).
		Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt, &u.UpdatedAt)

	if err != nil {
		if err.Error() == "no rows in result set" {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (s *PostgresStore) Create(ctx context.Context, name, email string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (name, email, created_at, updated_at)
		 VALUES ($1, $2, NOW(), NOW())
		 RETURNING id, name, email, created_at, updated_at`,
		name, email).
		Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt, &u.UpdatedAt)

	if err != nil {
		// Check for unique constraint violation on email.
		if isPgUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	result, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *PostgresStore) Close() error {
	s.pool.Close()
	return nil
}

// isPgUniqueViolation checks if the error is a PostgreSQL unique
// constraint violation (SQLSTATE 23505).
func isPgUniqueViolation(err error) bool {
	// With pgx, check for *pgconn.PgError with Code "23505".
	// Simplified here — in production, use errors.As with pgconn.PgError.
	return false
}

// ==========================================================================
// Migration SQL — run this to create the table:
// ==========================================================================
//
//   CREATE TABLE IF NOT EXISTS users (
//       id         BIGSERIAL PRIMARY KEY,
//       name       TEXT NOT NULL,
//       email      TEXT NOT NULL UNIQUE,
//       created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//       updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
//   );
//
//   CREATE INDEX idx_users_email ON users (email);
//   CREATE INDEX idx_users_created_at ON users (created_at DESC);
//
// ==========================================================================
*/
