// Package main is the analytics-worker binary.
//
// It drains the Redis "clicks" stream via a consumer group and batch-inserts
// raw events into ClickHouse. On startup it reclaims entries left pending by
// a crashed predecessor; delivery is at-least-once, and duplicate events are
// accepted as small analytics overcount (see README §6).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/mhockenbury/url-shortener/internal/events"
	ch "github.com/mhockenbury/url-shortener/internal/storage/clickhouse"
)

type appConfig struct {
	redisAddr       string
	chAddr          string
	chDatabase      string
	chUsername      string
	chPassword      string
	streamName      string
	groupName       string
	consumerName    string
	batchSize       int64
	blockDuration   time.Duration
	reclaimIdle     time.Duration
	shutdownGrace   time.Duration
	logLevel        slog.Level
}

func main() {
	if err := run(); err != nil {
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

	// Redis.
	rdb := goredis.NewClient(&goredis.Options{Addr: cfg.redisAddr})
	defer rdb.Close()

	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		pingCancel()
		return fmt.Errorf("redis ping: %w", err)
	}
	pingCancel()
	slog.Info("redis connected", "addr", cfg.redisAddr)

	// ClickHouse.
	connectCtx, connectCancel := context.WithTimeout(ctx, 5*time.Second)
	chClient, err := ch.NewClient(connectCtx, ch.Config{
		Addr:     cfg.chAddr,
		Database: cfg.chDatabase,
		Username: cfg.chUsername,
		Password: cfg.chPassword,
	})
	connectCancel()
	if err != nil {
		return fmt.Errorf("clickhouse connect: %w", err)
	}
	defer chClient.Close()
	slog.Info("clickhouse connected", "addr", cfg.chAddr)

	// Consumer.
	consumer := events.NewConsumer(rdb, cfg.streamName, cfg.groupName, cfg.consumerName)
	if err := consumer.EnsureGroup(ctx); err != nil {
		return fmt.Errorf("ensure group: %w", err)
	}
	slog.Info("consumer group ready",
		"stream", cfg.streamName, "group", cfg.groupName, "consumer", cfg.consumerName)

	// Reclaim anything a prior consumer left pending. Done before entering
	// the main loop so late-acking events don't stall new reads.
	if err := reclaimAndProcess(ctx, consumer, chClient, cfg); err != nil {
		// Log and continue — if reclaim fails we'd rather keep draining new
		// events than crash. The pending entries will still be there next start.
		slog.Warn("reclaim on startup failed", "err", err)
	}

	// Main loop.
	slog.Info("worker started", "batch_size", cfg.batchSize, "block", cfg.blockDuration)
	if err := runLoop(ctx, consumer, chClient, cfg); err != nil {
		return fmt.Errorf("worker loop: %w", err)
	}

	slog.Info("shutdown complete")
	return nil
}

// runLoop is the main read-insert-ack cycle. Returns nil on clean ctx cancellation.
func runLoop(ctx context.Context, consumer *events.Consumer, chClient *ch.Client, cfg appConfig) error {
	for {
		if err := ctx.Err(); err != nil {
			// Clean shutdown signal.
			return nil
		}

		batch, err := consumer.Read(ctx, cfg.batchSize, cfg.blockDuration)
		if err != nil {
			// Context cancelled during XREADGROUP: exit cleanly.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			slog.Error("read from stream", "err", err)
			// Brief backoff so we don't hot-loop on a persistent Redis error.
			if !sleepCtx(ctx, time.Second) {
				return nil
			}
			continue
		}
		if len(batch) == 0 {
			continue
		}

		if err := processBatch(ctx, chClient, batch); err != nil {
			slog.Error("process batch", "err", err, "size", len(batch))
			// Don't ack — leave the entries pending so a retry (or reclaim by
			// a fresh consumer) can pick them up. Brief backoff.
			if !sleepCtx(ctx, time.Second) {
				return nil
			}
			continue
		}

		ackIDs := make([]string, len(batch))
		for i, ev := range batch {
			ackIDs[i] = ev.ID
		}
		if err := consumer.Ack(ctx, ackIDs...); err != nil {
			// Ack failed: the batch is already in ClickHouse, so we'll see
			// duplicates on the reclaim path. That's acceptable overcount.
			slog.Error("ack batch", "err", err, "size", len(batch))
			continue
		}

		slog.Debug("batch processed", "size", len(batch))
	}
}

// reclaimAndProcess pulls entries left pending by a prior consumer, writes
// them to ClickHouse, and acks. Called once at startup.
func reclaimAndProcess(ctx context.Context, consumer *events.Consumer, chClient *ch.Client, cfg appConfig) error {
	reclaimed, err := consumer.ReclaimPending(ctx, cfg.reclaimIdle, cfg.batchSize)
	if err != nil {
		return fmt.Errorf("reclaim: %w", err)
	}
	if len(reclaimed) == 0 {
		return nil
	}
	slog.Info("reclaimed pending entries", "count", len(reclaimed))

	if err := processBatch(ctx, chClient, reclaimed); err != nil {
		return fmt.Errorf("process reclaimed: %w", err)
	}

	ackIDs := make([]string, len(reclaimed))
	for i, ev := range reclaimed {
		ackIDs[i] = ev.ID
	}
	if err := consumer.Ack(ctx, ackIDs...); err != nil {
		return fmt.Errorf("ack reclaimed: %w", err)
	}
	return nil
}

// processBatch converts stream events to ClickHouse rows and inserts them.
func processBatch(ctx context.Context, chClient *ch.Client, batch []events.ClickEvent) error {
	rows := make([]ch.ClickEvent, len(batch))
	for i, ev := range batch {
		rows[i] = ch.ClickEvent{
			ShortCode: ev.ShortCode,
			Timestamp: ev.Timestamp,
			Referrer:  ev.Referrer,
			UserAgent: ev.UserAgent,
		}
	}
	return chClient.Insert(ctx, rows)
}

// sleepCtx sleeps for d or until ctx is done, whichever comes first.
// Returns true if the sleep completed, false if interrupted.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func loadConfig() appConfig {
	hostname, _ := os.Hostname()
	defaultConsumer := fmt.Sprintf("%s-%d", hostname, os.Getpid())

	return appConfig{
		redisAddr:     envOr("REDIS_ADDR", "localhost:6379"),
		chAddr:        envOr("CLICKHOUSE_ADDR", "localhost:9000"),
		chDatabase:    envOr("CLICKHOUSE_DATABASE", "shortener"),
		chUsername:    envOr("CLICKHOUSE_USERNAME", "shortener"),
		chPassword:    envOr("CLICKHOUSE_PASSWORD", "shortener"),
		streamName:    envOr("STREAM_NAME", events.DefaultStream),
		groupName:     envOr("CONSUMER_GROUP", events.DefaultGroup),
		consumerName:  envOr("CONSUMER_NAME", defaultConsumer),
		batchSize:     envInt64("BATCH_SIZE", 1000),
		blockDuration: envDuration("BLOCK_DURATION", time.Second),
		reclaimIdle:   envDuration("RECLAIM_IDLE", 30*time.Second),
		shutdownGrace: envDuration("SHUTDOWN_GRACE", 10*time.Second),
		logLevel:      envLogLevel("LOG_LEVEL", slog.LevelInfo),
	}
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
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
