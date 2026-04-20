package shortener

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Allocator hands out monotonically increasing uint64 IDs.
//
// It reserves ranges of IDs from the Postgres `id_allocator` table in batches
// and serves individual IDs from the local range without touching the DB.
// When the range is exhausted, a single atomic UPDATE reserves the next batch.
//
// Concurrency: safe for use by multiple goroutines. A single mutex guards both
// the local range and the refill round-trip; under heavy contention a
// larger batch amortizes the refill cost.
type Allocator struct {
	pool      *pgxpool.Pool
	name      string
	batchSize uint64

	mu           sync.Mutex
	next         uint64 // next ID to hand out
	endExclusive uint64 // one past the last reserved ID
}

// NewAllocator constructs an allocator. The `id_allocator` row for `name`
// must already exist (seeded by migrations/0001_init.sql).
func NewAllocator(pool *pgxpool.Pool, name string, batchSize uint64) (*Allocator, error) {
	if pool == nil {
		return nil, errors.New("allocator: pool is nil")
	}
	if name == "" {
		return nil, errors.New("allocator: name is empty")
	}
	if batchSize == 0 {
		return nil, errors.New("allocator: batchSize must be > 0")
	}
	return &Allocator{pool: pool, name: name, batchSize: batchSize}, nil
}

// Next returns the next reserved ID, refilling the local range if needed.
func (a *Allocator) Next(ctx context.Context) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.next >= a.endExclusive {
		if err := a.refillLocked(ctx); err != nil {
			return 0, err
		}
	}
	id := a.next
	a.next++
	return id, nil
}

// refillLocked reserves the next batch of IDs atomically. Caller must hold a.mu.
//
// The SQL is a single statement: advance next_id by batchSize and return the
// *previous* next_id (the start of our reserved range). Postgres guarantees
// atomicity of UPDATE ... RETURNING without needing an explicit transaction
// or SELECT FOR UPDATE.
func (a *Allocator) refillLocked(ctx context.Context) error {
	const q = `
        UPDATE id_allocator
        SET next_id = next_id + $2
        WHERE name = $1
        RETURNING next_id - $2`

	var start uint64
	err := a.pool.QueryRow(ctx, q, a.name, a.batchSize).Scan(&start)
	if err != nil {
		return fmt.Errorf("allocator: refill %q: %w", a.name, err)
	}
	a.next = start
	a.endExclusive = start + a.batchSize
	return nil
}

// Remaining returns how many IDs are left in the current local range.
// Intended for tests and metrics; not part of the normal call path.
func (a *Allocator) Remaining() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.next >= a.endExclusive {
		return 0
	}
	return a.endExclusive - a.next
}
