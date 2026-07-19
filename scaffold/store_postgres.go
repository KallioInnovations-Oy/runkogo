package main

// ==========================================================================
// PostgresStore — production database implementation
// ==========================================================================
//
// This file COMPILES. It uses only database/sql from the standard library,
// so the scaffold stays dependency-free and CI type-checks this code on
// every push. An earlier version of this file lived inside a block comment;
// it had three bugs that no one could see, including a column type that
// could never scan into the User struct. Commented-out code is not a
// reference implementation — it is a guess that has never been checked.
//
// To use it:
//
//  1. Add a driver:  go get github.com/jackc/pgx/v5
//  2. Import it for its side effects in main.go:
//     import _ "github.com/jackc/pgx/v5/stdlib"
//  3. Run the migration at the bottom of this file.
//  4. In main(), swap the store:
//
//     db, err := NewPostgresStore(ctx, "pgx", app.Config.MustGet("DATABASE_URL"))
//     if err != nil { ... }
//     store = db
//
// Nothing else changes — handlers depend on the UserStore interface, not on
// any implementation.
//
// Connection string:
//
//	DATABASE_URL=postgres://user:pass@localhost:5432/runko?sslmode=disable

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PostgresStore implements UserStore backed by PostgreSQL.
type PostgresStore struct {
	db *sql.DB
}

// Compile-time proof that PostgresStore satisfies UserStore. This is the
// point of the file compiling at all: change the interface and the build
// breaks here, instead of the mismatch waiting inside a comment for
// whoever eventually uncomments it.
var _ UserStore = (*PostgresStore)(nil)

// NewPostgresStore opens the pool and verifies connectivity. Call Close
// during shutdown — the scaffold wires that up in an OnShutdown hook.
//
// driverName must match a driver registered by an imported package, e.g.
// "pgx" for github.com/jackc/pgx/v5/stdlib.
func NewPostgresStore(ctx context.Context, driverName, dsn string) (*PostgresStore, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Pool tuning. The default MaxIdleConns of 2 is the single most common
	// throughput bottleneck in Go services talking to a database.
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	// Fail fast: a store that cannot reach its database should not let the
	// app report itself ready.
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("database ping failed: %w", err)
	}

	return &PostgresStore{db: db}, nil
}

// ListPage returns one page of users plus the total count.
//
// The limit and offset are pushed down to the query rather than applied in
// the handler. Fetching every row and slicing it in Go is the thing that
// works fine on a demo dataset and falls over in production. Note that
// OFFSET pagination degrades at large offsets — Postgres still walks the
// skipped rows; switch to keyset pagination if that becomes a problem.
func (s *PostgresStore) ListPage(ctx context.Context, limit, offset int) ([]User, int, error) {
	// Clamp before the values reach make() or the query. A negative limit
	// would panic on the capacity argument below, before Postgres ever got
	// a chance to reject it.
	if limit < 0 {
		limit = 0
	}
	if offset < 0 {
		offset = 0
	}

	// The count and the page run in separate snapshots, so a concurrent
	// insert can make total disagree with the rows returned. That is
	// acceptable for a listing; if you need them consistent, run both
	// inside a REPEATABLE READ transaction.
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, email, created_at, updated_at
		   FROM users
		  ORDER BY created_at DESC
		  LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	users := make([]User, 0, limit)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	// rows.Err reports failures that ended iteration early. Skipping it
	// silently truncates result sets on a mid-stream connection error.
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate users: %w", err)
	}
	return users, total, nil
}

func (s *PostgresStore) GetByID(ctx context.Context, id string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, email, created_at, updated_at
		   FROM users
		  WHERE id = $1`, id).
		Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt, &u.UpdatedAt)

	// errors.Is against the sentinel, never a string comparison on the
	// error text — that breaks silently on any driver or version change.
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (s *PostgresStore) Create(ctx context.Context, name, email string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO users (name, email)
		 VALUES ($1, $2)
		 RETURNING id, name, email, created_at, updated_at`,
		name, email).
		Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt, &u.UpdatedAt)

	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// sqlStateError is implemented by driver errors that expose a SQLSTATE
// code, including pgx's *pgconn.PgError. Matching on the interface rather
// than importing a driver keeps this file dependency-free and works across
// drivers that follow the same convention.
type sqlStateError interface {
	SQLState() string
}

// isUniqueViolation reports whether err is a PostgreSQL unique constraint
// violation, SQLSTATE 23505.
//
// The previous version of this function returned false unconditionally with
// a comment calling it "simplified". The effect was that ErrConflict was
// never returned, the handler's conflict branch was dead code, and every
// duplicate email surfaced to the client as a 500.
func isUniqueViolation(err error) bool {
	var sqlErr sqlStateError
	if errors.As(err, &sqlErr) {
		return sqlErr.SQLState() == "23505"
	}
	return false
}

// ==========================================================================
// Migration
// ==========================================================================
//
// id is TEXT, not BIGSERIAL. User.ID is a string, and a bigint column can
// never scan into it — the mismatch would fail at runtime on every single
// query. Match the column type to the struct field.
//
//	CREATE TABLE IF NOT EXISTS users (
//	    id         TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
//	    name       TEXT NOT NULL,
//	    email      TEXT NOT NULL UNIQUE,
//	    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
//	);
//
//	-- No index on email: the UNIQUE constraint already creates one.
//	CREATE INDEX IF NOT EXISTS idx_users_created_at ON users (created_at DESC);
//
// gen_random_uuid() is built in from PostgreSQL 13. On older versions,
// enable pgcrypto or generate IDs in the application.
