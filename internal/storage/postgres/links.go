// Package postgres holds the Postgres-backed implementation of link storage.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors returned by LinkStore. Callers match on these to map to
// HTTP status codes without coupling to pgx error types.
var (
	ErrNotFound       = errors.New("link not found")
	ErrAliasTaken     = errors.New("short_code already taken")
	ErrIDAlreadyTaken = errors.New("id already taken")
)

// Postgres unique-violation SQLSTATE.
const pgUniqueViolation = "23505"

// Link is the row shape stored in the `links` table. Fields mirror the
// schema 1:1; conversions happen at layer boundaries, not here.
type Link struct {
	ID        uint64
	ShortCode string
	LongURL   string
	CreatedAt time.Time
	ExpiresAt *time.Time // nil = no expiry
	CreatedBy *string    // nil = anonymous
}

// LinkStore is the narrow persistence interface over the `links` table.
// Higher layers depend on this interface, not on *pgxpool.Pool.
type LinkStore struct {
	pool *pgxpool.Pool
}

// NewLinkStore wraps a pgx pool. The pool must already be configured and pingable.
func NewLinkStore(pool *pgxpool.Pool) *LinkStore {
	return &LinkStore{pool: pool}
}

// Insert writes a new link with an explicit id (from the allocator) and
// short_code (base62-encoded id). Returns ErrIDAlreadyTaken or ErrAliasTaken
// if the respective unique constraint is violated.
func (s *LinkStore) Insert(ctx context.Context, l Link) error {
	const q = `
        INSERT INTO links (id, short_code, long_url, created_at, expires_at, created_by)
        VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6)`

	var createdAt any
	if !l.CreatedAt.IsZero() {
		createdAt = l.CreatedAt
	}

	_, err := s.pool.Exec(ctx, q,
		l.ID, l.ShortCode, l.LongURL, createdAt, l.ExpiresAt, l.CreatedBy)
	if err != nil {
		return mapUniqueViolation(err)
	}
	return nil
}

// InsertAlias writes a link with a caller-supplied short_code. Counter-allocated
// IDs and custom aliases share the same short_code namespace (UNIQUE index),
// but aliases need their own id — currently a placeholder until we decide on
// a reserved alias ID range (tracked in docs/tradeoffs.md).
//
// For now: callers must pass an id from the allocator just like regular inserts.
// Alias behavior differs only in that short_code is not derived from id.
func (s *LinkStore) InsertAlias(ctx context.Context, l Link) error {
	// Same statement as Insert; the distinction is in how the caller chose short_code.
	// Kept as a separate method so the call site reads clearly and so future
	// divergence (reserved ID ranges, permission checks) has a place to live.
	return s.Insert(ctx, l)
}

// Lookup returns the link for a short_code. Returns ErrNotFound if no row exists.
// Expiry is NOT enforced here — callers check ExpiresAt and translate to 410.
// Keeping expiry-check out of the query lets the cache layer serve expired
// rows it hasn't invalidated yet, with the HTTP layer making the final call.
func (s *LinkStore) Lookup(ctx context.Context, shortCode string) (Link, error) {
	const q = `
        SELECT id, short_code, long_url, created_at, expires_at, created_by
        FROM links
        WHERE short_code = $1`

	var l Link
	err := s.pool.QueryRow(ctx, q, shortCode).Scan(
		&l.ID, &l.ShortCode, &l.LongURL, &l.CreatedAt, &l.ExpiresAt, &l.CreatedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Link{}, ErrNotFound
		}
		return Link{}, fmt.Errorf("lookup %q: %w", shortCode, err)
	}
	return l, nil
}

// mapUniqueViolation translates a Postgres unique-constraint error into the
// appropriate sentinel based on which constraint fired. Other errors pass through.
func mapUniqueViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		return err
	}
	switch pgErr.ConstraintName {
	case "links_pkey":
		return ErrIDAlreadyTaken
	case "links_short_code_key":
		return ErrAliasTaken
	default:
		return err
	}
}
