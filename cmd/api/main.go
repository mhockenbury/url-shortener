// Package main is the url-shortener API binary.
//
// Config is read from env vars; see config() for defaults. The binary
// opens a Postgres pool, a Redis client, wires the LinkService, and
// serves the HTTP router until SIGINT/SIGTERM, then shuts down gracefully.
//
//	@title						url-shortener API
//	@version					0.1
//	@description				bit.ly-style URL shortener — first subproject in sysdesign-lab. See docs/ for full architecture + tradeoffs.
//	@BasePath					/
//	@schemes					http
//	@contact.name				matt hockenbury
//	@contact.url				https://github.com/mhockenbury/url-shortener
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	myhttp "github.com/mhockenbury/url-shortener/internal/http"
	"github.com/mhockenbury/url-shortener/internal/shortener"
	ch "github.com/mhockenbury/url-shortener/internal/storage/clickhouse"
	pg "github.com/mhockenbury/url-shortener/internal/storage/postgres"
	cacheredis "github.com/mhockenbury/url-shortener/internal/storage/redis"
)

type appConfig struct {
	databaseURL    string
	redisAddr      string
	chAddr         string
	chDatabase     string
	chUsername     string
	chPassword     string
	httpAddr       string
	baseURL        string
	allocName      string
	idBatchSize    uint64
	cacheTTL       time.Duration
	shutdownGrace  time.Duration
	logLevel       slog.Level
}

func main() {
	if err := run(); err != nil {
		// slog may not be configured if we failed that early — fall back to stderr.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := loadConfig()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Postgres pool.
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("pgxpool.New: %w", err)
	}
	defer pool.Close()

	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := pool.Ping(pingCtx); err != nil {
		pingCancel()
		return fmt.Errorf("postgres ping: %w", err)
	}
	pingCancel()
	slog.Info("postgres connected", "addr", redactDSN(cfg.databaseURL))

	// Redis client.
	rdb := goredis.NewClient(&goredis.Options{Addr: cfg.redisAddr})
	defer rdb.Close()

	pingCtx, pingCancel = context.WithTimeout(ctx, 5*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		pingCancel()
		return fmt.Errorf("redis ping: %w", err)
	}
	pingCancel()
	slog.Info("redis connected", "addr", cfg.redisAddr)

	// ClickHouse client (used by the stats endpoint; worker owns its own).
	chConnectCtx, chCancel := context.WithTimeout(ctx, 5*time.Second)
	chClient, err := ch.NewClient(chConnectCtx, ch.Config{
		Addr:     cfg.chAddr,
		Database: cfg.chDatabase,
		Username: cfg.chUsername,
		Password: cfg.chPassword,
	})
	chCancel()
	if err != nil {
		return fmt.Errorf("clickhouse connect: %w", err)
	}
	defer chClient.Close()
	slog.Info("clickhouse connected", "addr", cfg.chAddr)

	// Domain + storage wiring.
	alloc, err := shortener.NewAllocator(pool, cfg.allocName, cfg.idBatchSize)
	if err != nil {
		return fmt.Errorf("allocator: %w", err)
	}
	links := pg.NewLinkStore(pool)
	cache := cacheredis.NewClient(rdb)
	svc := shortener.NewLinkService(alloc, links, cache, chClient, nil, cfg.cacheTTL)

	// HTTP layer.
	handlers := myhttp.NewHandlers(
		svc,
		myhttp.DefaultResolver,
		cfg.baseURL,
		func(ctx context.Context) error { return pool.Ping(ctx) },
		func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
	)

	srv := &nethttp.Server{
		Addr:              cfg.httpAddr,
		Handler:           myhttp.Router(handlers),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Run the server in a goroutine so we can listen for signals in main.
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", cfg.httpAddr, "base_url", cfg.baseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining")
	}

	// Graceful shutdown: stop accepting new conns, wait for in-flight.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	slog.Info("shutdown complete")
	return nil
}

// loadConfig reads env vars with defaults suited to the local docker-compose
// setup. No validation beyond what each env getter does; invalid values
// (e.g., malformed duration) cause the process to exit at startup.
func loadConfig() appConfig {
	return appConfig{
		databaseURL:   envOr("DATABASE_URL", "postgres://shortener:shortener@localhost:5432/shortener"),
		redisAddr:     envOr("REDIS_ADDR", "localhost:6379"),
		chAddr:        envOr("CLICKHOUSE_ADDR", "localhost:9000"),
		chDatabase:    envOr("CLICKHOUSE_DATABASE", "shortener"),
		chUsername:    envOr("CLICKHOUSE_USERNAME", "shortener"),
		chPassword:    envOr("CLICKHOUSE_PASSWORD", "shortener"),
		httpAddr:      envOr("HTTP_ADDR", ":8080"),
		baseURL:       envOr("BASE_URL", "http://localhost:8080"),
		allocName:     envOr("ID_ALLOCATOR_NAME", "links"),
		idBatchSize:   envUint("ID_BATCH_SIZE", 1000),
		cacheTTL:      envDuration("CACHE_TTL", time.Hour),
		shutdownGrace: envDuration("SHUTDOWN_GRACE", 15*time.Second),
		logLevel:      envLogLevel("LOG_LEVEL", slog.LevelInfo),
	}
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envUint(key string, fallback uint64) uint64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid %s=%q: %v; using %d\n", key, v, err, fallback)
			return fallback
		}
		return n
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid %s=%q: %v; using %s\n", key, v, err, fallback)
			return fallback
		}
		return d
	}
	return fallback
}

func envLogLevel(key string, fallback slog.Level) slog.Level {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		fmt.Fprintf(os.Stderr, "invalid %s=%q; using %s\n", key, v, fallback)
		return fallback
	}
}

// redactDSN strips the password from a Postgres connection string for log
// output. Handles the common `postgres://user:pass@host/db` form; falls back
// to the original string if the shape isn't recognizable (better to log
// awkwardly than to crash on parse).
func redactDSN(dsn string) string {
	// Find "://" then the first "@" after it; the password sits between the
	// colon after the user and the "@".
	scheme := strings.Index(dsn, "://")
	at := strings.Index(dsn, "@")
	if scheme < 0 || at < 0 || at <= scheme+3 {
		return dsn
	}
	userinfo := dsn[scheme+3 : at]
	colon := strings.Index(userinfo, ":")
	if colon < 0 {
		return dsn
	}
	return dsn[:scheme+3] + userinfo[:colon] + ":***" + dsn[at:]
}
