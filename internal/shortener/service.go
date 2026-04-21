package shortener

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"

	ch "github.com/mhockenbury/url-shortener/internal/storage/clickhouse"
	pg "github.com/mhockenbury/url-shortener/internal/storage/postgres"
	cacheredis "github.com/mhockenbury/url-shortener/internal/storage/redis"
)

// Sentinel errors surfaced to handlers. Handlers map these to HTTP status codes.
var (
	ErrNotFound          = errors.New("link not found")
	ErrExpired           = errors.New("link expired")
	ErrAliasTaken        = errors.New("alias already taken")
	ErrStatsUnavailable  = errors.New("stats backend not configured")
)

// DefaultCacheTTL is how long cached short_code -> long_url entries live in
// Redis. Matches the 1h target in docs/architecture.md.
const DefaultCacheTTL = time.Hour

// Clock is a tiny abstraction over time.Now so tests can exercise expiry
// without sleeping. Default is wall-clock.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

// DefaultClock is the production clock.
var DefaultClock Clock = wallClock{}

// LinkService orchestrates link create/lookup/stats across allocator,
// Postgres, Redis cache, clicks event stream, and ClickHouse analytics.
// Handlers depend on this type; this type depends on the storage adapters.
//
// Singleflight collapses concurrent cache misses on the same short_code to
// a single Postgres query — the "thundering herd" mitigation documented in
// docs/tradeoffs.md.
type LinkService struct {
	alloc      *Allocator
	links      *pg.LinkStore
	redis      *cacheredis.Client
	clickhouse *ch.Client
	clock      Clock
	cacheTTL   time.Duration
	sf         singleflight.Group
}

// NewLinkService wires the service. `redis` may be nil for tests that want
// to exercise the DB path only; if nil, cache ops are skipped silently.
// `clickhouse` may be nil if stats queries aren't wanted — Stats will
// return ErrStatsUnavailable in that case. `clock` may be nil, defaulting
// to DefaultClock.
func NewLinkService(
	alloc *Allocator,
	links *pg.LinkStore,
	redis *cacheredis.Client,
	clickhouse *ch.Client,
	clock Clock,
	cacheTTL time.Duration,
) *LinkService {
	if clock == nil {
		clock = DefaultClock
	}
	if cacheTTL <= 0 {
		cacheTTL = DefaultCacheTTL
	}
	return &LinkService{
		alloc:      alloc,
		links:      links,
		redis:      redis,
		clickhouse: clickhouse,
		clock:      clock,
		cacheTTL:   cacheTTL,
	}
}

// CreateInput carries the fields the handler accepts.
type CreateInput struct {
	LongURL   string
	Alias     string     // empty means auto-generate from allocator
	ExpiresAt *time.Time // nil means no expiry
	CreatedBy *string    // nil means anonymous
}

// CreateResult is what Create returns: the stored short_code and its id.
type CreateResult struct {
	ID        uint64
	ShortCode string
	LongURL   string
	ExpiresAt *time.Time
}

// Create inserts a new link. With an empty Alias it allocates an ID and
// derives the short_code via base62. With a non-empty Alias it uses the
// alias as short_code directly; a collision returns ErrAliasTaken.
//
// Input URL validation (scheme, SSRF guard, length) is the HTTP layer's
// responsibility — this method trusts its caller.
func (s *LinkService) Create(ctx context.Context, in CreateInput) (CreateResult, error) {
	id, err := s.alloc.Next(ctx)
	if err != nil {
		return CreateResult{}, fmt.Errorf("allocate id: %w", err)
	}

	code := in.Alias
	if code == "" {
		code = Encode(id)
	}

	link := pg.Link{
		ID:        id,
		ShortCode: code,
		LongURL:   in.LongURL,
		ExpiresAt: in.ExpiresAt,
		CreatedBy: in.CreatedBy,
	}

	insert := s.links.Insert
	if in.Alias != "" {
		insert = s.links.InsertAlias
	}
	if err := insert(ctx, link); err != nil {
		if errors.Is(err, pg.ErrAliasTaken) {
			return CreateResult{}, ErrAliasTaken
		}
		return CreateResult{}, fmt.Errorf("insert link: %w", err)
	}

	return CreateResult{
		ID:        id,
		ShortCode: code,
		LongURL:   in.LongURL,
		ExpiresAt: in.ExpiresAt,
	}, nil
}

// LookupResult carries what the redirect handler needs to serve the 3xx or
// translate to 404/410.
type LookupResult struct {
	LongURL   string
	ExpiresAt *time.Time // nil = no expiry; populated when cache miss falls through to DB
	FromCache bool
}

// Lookup resolves short_code -> long_url. Read path:
//   1. Try Redis. On hit, return without touching Postgres or checking expiry
//      (the cache is populated only with non-expired rows; brief staleness on
//      recently-expired rows is accepted — see docs/tradeoffs.md).
//   2. On miss, fall through to Postgres via singleflight so concurrent
//      misses of the same code collapse to one query.
//   3. If the DB row is expired, return ErrExpired without caching.
//   4. Otherwise cache the result and return.
func (s *LinkService) Lookup(ctx context.Context, shortCode string) (LookupResult, error) {
	if s.redis != nil {
		url, err := s.redis.GetLongURL(ctx, shortCode)
		if err == nil {
			return LookupResult{LongURL: url, FromCache: true}, nil
		}
		if !errors.Is(err, cacheredis.ErrCacheMiss) {
			// Cache errors degrade to DB fallback. Don't fail the whole redirect
			// because Redis hiccupped — the DB is the source of truth.
			// TODO: structured logging once we wire zerolog.
			_ = err
		}
	}

	v, err, _ := s.sf.Do(shortCode, func() (any, error) {
		return s.dbLookup(ctx, shortCode)
	})
	if err != nil {
		return LookupResult{}, err
	}
	return v.(LookupResult), nil
}

// dbLookup performs the Postgres fetch and cache population. Extracted so
// singleflight's callback can stay small.
func (s *LinkService) dbLookup(ctx context.Context, shortCode string) (LookupResult, error) {
	link, err := s.links.Lookup(ctx, shortCode)
	if err != nil {
		if errors.Is(err, pg.ErrNotFound) {
			return LookupResult{}, ErrNotFound
		}
		return LookupResult{}, fmt.Errorf("db lookup: %w", err)
	}
	if link.ExpiresAt != nil && !link.ExpiresAt.After(s.clock.Now()) {
		return LookupResult{}, ErrExpired
	}

	if s.redis != nil {
		ttl := s.cacheTTL
		// Cap TTL at remaining lifetime so we don't cache a row past its expiry.
		if link.ExpiresAt != nil {
			remaining := link.ExpiresAt.Sub(s.clock.Now())
			if remaining > 0 && remaining < ttl {
				ttl = remaining
			}
		}
		// Best-effort: cache-set failure should not break the redirect.
		_ = s.redis.SetLongURL(ctx, shortCode, link.LongURL, ttl)
	}

	return LookupResult{
		LongURL:   link.LongURL,
		ExpiresAt: link.ExpiresAt,
		FromCache: false,
	}, nil
}

// StatsResult is the service-level view of analytics. Mirrors the adapter
// shape so the handler doesn't need to import the storage package.
type StatsResult struct {
	Total  uint64         `json:"total"`
	Hourly []HourlyBucket `json:"hourly"`
}

// HourlyBucket is one row of the stats response — count of clicks in a
// given hour. Returned in chronological order.
type HourlyBucket struct {
	Hour  time.Time `json:"hour"`
	Count uint64    `json:"count"`
}

// Stats returns click analytics for shortCode over [since, until).
// Returns ErrStatsUnavailable if the ClickHouse client isn't configured.
// The handler chooses the time window; the service does not impose one.
func (s *LinkService) Stats(ctx context.Context, shortCode string, since, until time.Time) (StatsResult, error) {
	if s.clickhouse == nil {
		return StatsResult{}, ErrStatsUnavailable
	}
	chRes, err := s.clickhouse.Stats(ctx, shortCode, since, until)
	if err != nil {
		return StatsResult{}, fmt.Errorf("stats: %w", err)
	}
	out := StatsResult{
		Total:  chRes.Total,
		Hourly: make([]HourlyBucket, len(chRes.Hourly)),
	}
	for i, b := range chRes.Hourly {
		out.Hourly[i] = HourlyBucket{Hour: b.Hour, Count: b.Count}
	}
	return out, nil
}

// PublishClick emits a click event best-effort. Errors are returned for
// the caller to log; the redirect path should not block on this.
func (s *LinkService) PublishClick(ctx context.Context, shortCode, referrer, userAgent string) error {
	if s.redis == nil {
		return nil
	}
	return s.redis.PublishClick(ctx, cacheredis.ClickEvent{
		ShortCode: shortCode,
		Timestamp: s.clock.Now(),
		Referrer:  referrer,
		UserAgent: userAgent,
	})
}
