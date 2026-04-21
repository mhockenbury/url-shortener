package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	myhttp "github.com/mhockenbury/url-shortener/internal/http"
	"github.com/mhockenbury/url-shortener/internal/shortener"
	ch "github.com/mhockenbury/url-shortener/internal/storage/clickhouse"
	pg "github.com/mhockenbury/url-shortener/internal/storage/postgres"
	cacheredis "github.com/mhockenbury/url-shortener/internal/storage/redis"
)

// Integration-style tests: real handlers, real service, real Postgres, real
// Redis. Skipped if either is unreachable so `go test ./...` on a fresh
// checkout still passes.

const (
	defaultPGDSN     = "postgres://shortener:shortener@localhost:5432/shortener"
	defaultRedisAddr = "localhost:6379"
	defaultCHAddr    = "localhost:9000"
)

type testEnv struct {
	pool   *pgxpool.Pool
	rdb    *goredis.Client
	ch     *ch.Client
	svc    *shortener.LinkService
	h      *myhttp.Handlers
	alloc  *shortener.Allocator
	links  *pg.LinkStore
	cache  *cacheredis.Client
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultPGDSN
	}
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = defaultRedisAddr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil || pool.Ping(ctx) != nil {
		if pool != nil {
			pool.Close()
		}
		t.Skipf("postgres unreachable (dsn=%s): %v", dsn, err)
	}

	rdb := goredis.NewClient(&goredis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		pool.Close()
		t.Skipf("redis unreachable (addr=%s): %v", addr, err)
	}

	chAddr := os.Getenv("CLICKHOUSE_ADDR")
	if chAddr == "" {
		chAddr = defaultCHAddr
	}
	chClient, err := ch.NewClient(ctx, ch.Config{
		Addr:     chAddr,
		Database: "shortener",
		Username: "shortener",
		Password: "shortener",
	})
	if err != nil {
		_ = rdb.Close()
		pool.Close()
		t.Skipf("clickhouse unreachable (addr=%s): %v", chAddr, err)
	}

	// Per-test allocator name AND per-test starting id. The id must not
	// collide with rows left behind by earlier test runs on the shared
	// `links` table, so we seed next_id from the nanosecond clock.
	now := time.Now().UnixNano()
	allocName := fmt.Sprintf("test_http_%d", now)
	_, err = pool.Exec(ctx,
		`INSERT INTO id_allocator (name, next_id) VALUES ($1, $2)
		 ON CONFLICT (name) DO UPDATE SET next_id = EXCLUDED.next_id`, allocName, now)
	if err != nil {
		t.Fatalf("seed allocator: %v", err)
	}

	alloc, err := shortener.NewAllocator(pool, allocName, 100)
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	links := pg.NewLinkStore(pool)
	cache := cacheredis.NewClient(rdb)
	svc := shortener.NewLinkService(alloc, links, cache, chClient, nil, 10*time.Second)

	// A resolver that accepts our test hosts as public addresses; real DNS
	// would be too slow and flaky for tests.
	resolver := fakeResolver{answers: map[string][]net.IP{
		"example.com":     {net.ParseIP("93.184.216.34")},
		"example.org":     {net.ParseIP("93.184.216.35")},
		"test-host.local": {net.ParseIP("93.184.216.36")},
	}}

	pingPG := func(ctx context.Context) error { return pool.Ping(ctx) }
	pingRedis := func(ctx context.Context) error { return rdb.Ping(ctx).Err() }

	h := myhttp.NewHandlers(svc, resolver, "http://test", pingPG, pingRedis)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM id_allocator WHERE name=$1`, allocName)
		_ = chClient.Close()
		_ = rdb.Close()
		pool.Close()
	})

	return &testEnv{
		pool:  pool,
		rdb:   rdb,
		ch:    chClient,
		svc:   svc,
		h:     h,
		alloc: alloc,
		links: links,
		cache: cache,
	}
}

// fakeResolver mirrors the one in validate_test.go but is redeclared here
// because that one lives in package http (internal) and these tests are
// in package http_test. Duplication is cheaper than exporting a test helper.
type fakeResolver struct {
	answers map[string][]net.IP
}

func (f fakeResolver) LookupIP(host string) ([]net.IP, error) {
	if ips, ok := f.answers[host]; ok {
		return ips, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// --- POST /shorten ---

func TestShorten_SuccessReturnsCreatedCode(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	body := strings.NewReader(`{"url": "https://example.com/path"}`)
	req := httptest.NewRequest(http.MethodPost, "/shorten", body)
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ShortCode string `json:"short_code"`
		ShortURL  string `json:"short_url"`
		LongURL   string `json:"long_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ShortCode == "" || resp.LongURL != "https://example.com/path" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if !strings.HasSuffix(resp.ShortURL, "/"+resp.ShortCode) {
		t.Errorf("short_url = %q should end with %q", resp.ShortURL, "/"+resp.ShortCode)
	}
}

func TestShorten_RejectsInvalidScheme(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	req := httptest.NewRequest(http.MethodPost, "/shorten",
		strings.NewReader(`{"url":"javascript:alert(1)"}`))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestShorten_RejectsPrivateIP(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	req := httptest.NewRequest(http.MethodPost, "/shorten",
		strings.NewReader(`{"url":"http://10.0.0.1/"}`))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
}

func TestShorten_RejectsInvalidAlias(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	req := httptest.NewRequest(http.MethodPost, "/shorten",
		strings.NewReader(`{"url":"https://example.com/","alias":"bad alias!"}`))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestShorten_DuplicateAliasReturns409(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	alias := fmt.Sprintf("a%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"url":"https://example.com/","alias":%q}`, alias)

	req := httptest.NewRequest(http.MethodPost, "/shorten", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/shorten", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate create status = %d, want 409", w.Code)
	}
}

// --- GET /{code} ---

func TestRedirect_HitsAndPublishesClick(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	created := createViaHandler(t, r, "https://example.org/target", "")

	req := httptest.NewRequest(http.MethodGet, "/"+created.ShortCode, nil)
	req.Header.Set("User-Agent", "test-agent")
	req.Header.Set("Referer", "https://ref.example.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://example.org/target" {
		t.Errorf("Location = %q", loc)
	}

	// Click is emitted on a goroutine; give it a moment to land.
	time.Sleep(100 * time.Millisecond)
	streamLen, err := env.cache.StreamLen(context.Background())
	if err != nil {
		t.Fatalf("StreamLen: %v", err)
	}
	if streamLen < 1 {
		t.Errorf("stream length = %d, want >= 1 after redirect", streamLen)
	}
}

func TestRedirect_NotFoundReturns404(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	req := httptest.NewRequest(http.MethodGet, "/doesnotexist", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRedirect_ExpiredReturns410(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	// Create with a future expiry via the handler, then force-expire in DB.
	created := createViaHandler(t, r,
		"https://example.com/will-expire",
		"",
		withExpiresIn(time.Hour))

	_, err := env.pool.Exec(context.Background(),
		`UPDATE links SET expires_at = now() - interval '1 second' WHERE short_code = $1`,
		created.ShortCode)
	if err != nil {
		t.Fatalf("force expire: %v", err)
	}
	// Expired rows may still be cached from creation; evict.
	_ = env.cache.DeleteLongURL(context.Background(), created.ShortCode)

	req := httptest.NewRequest(http.MethodGet, "/"+created.ShortCode, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusGone {
		t.Errorf("status = %d, want 410. body=%s", w.Code, w.Body.String())
	}
}

func TestRedirect_InvalidCodeReturns400(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	req := httptest.NewRequest(http.MethodGet, "/not-valid!", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- GET /healthz ---

func TestHealth_AllUpReturns200(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
}

// --- GET /stats/{code} ---

func TestStats_ReturnsTotalAndHourly(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	// Create a link, then seed click rows directly in ClickHouse so we
	// control the hour distribution without relying on the async worker.
	created := createViaHandler(t, r, "https://example.org/stats-test", "")

	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Hour)
	events := []ch.ClickEvent{
		{ShortCode: created.ShortCode, Timestamp: base.Add(5 * time.Minute)},
		{ShortCode: created.ShortCode, Timestamp: base.Add(20 * time.Minute)},
		{ShortCode: created.ShortCode, Timestamp: base.Add(70 * time.Minute)},
	}
	if err := env.ch.Insert(context.Background(), events); err != nil {
		t.Fatalf("seed clicks: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/stats/"+created.ShortCode, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ShortCode string `json:"short_code"`
		Total     uint64 `json:"total"`
		Hourly    []struct {
			Hour  time.Time `json:"hour"`
			Count uint64    `json:"count"`
		} `json:"hourly"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ShortCode != created.ShortCode {
		t.Errorf("ShortCode = %q, want %q", resp.ShortCode, created.ShortCode)
	}
	if resp.Total != 3 {
		t.Errorf("Total = %d, want 3", resp.Total)
	}
	if len(resp.Hourly) != 2 {
		t.Fatalf("Hourly buckets = %d, want 2. got %+v", len(resp.Hourly), resp.Hourly)
	}
	if resp.Hourly[0].Count != 2 || resp.Hourly[1].Count != 1 {
		t.Errorf("bucket counts = %d,%d; want 2,1", resp.Hourly[0].Count, resp.Hourly[1].Count)
	}
}

func TestStats_UnknownCodeReturnsZero(t *testing.T) {
	// Unknown short_codes just return zeros — the handler doesn't validate
	// against Postgres, which keeps it cheap. Documented tradeoff.
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	code := fmt.Sprintf("nx%d", time.Now().UnixNano())
	req := httptest.NewRequest(http.MethodGet, "/stats/"+code, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Total uint64 `json:"total"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Total != 0 {
		t.Errorf("Total = %d, want 0", resp.Total)
	}
}

func TestStats_InvalidCodeReturns400(t *testing.T) {
	env := newTestEnv(t)
	r := myhttp.Router(env.h)

	req := httptest.NewRequest(http.MethodGet, "/stats/bad!code", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- helpers ---

type createOpts struct {
	expiresAt *time.Time
}

type createOpt func(*createOpts)

func withExpiresIn(d time.Duration) createOpt {
	t := time.Now().Add(d).UTC()
	return func(o *createOpts) { o.expiresAt = &t }
}

type createdLink struct {
	ShortCode string `json:"short_code"`
	ShortURL  string `json:"short_url"`
	LongURL   string `json:"long_url"`
}

func createViaHandler(t *testing.T, r http.Handler, longURL, alias string, opts ...createOpt) createdLink {
	t.Helper()
	var o createOpts
	for _, opt := range opts {
		opt(&o)
	}
	payload := map[string]any{"url": longURL}
	if alias != "" {
		payload["alias"] = alias
	}
	if o.expiresAt != nil {
		payload["expires_at"] = o.expiresAt
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/shorten", strings.NewReader(string(body)))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("createViaHandler status = %d body=%s", w.Code, w.Body.String())
	}
	var cl createdLink
	if err := json.Unmarshal(w.Body.Bytes(), &cl); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return cl
}
