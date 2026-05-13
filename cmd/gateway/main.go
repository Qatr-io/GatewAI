package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"kevent/gateway/internal/cache"
	"kevent/gateway/internal/cache/semantic"
	"kevent/gateway/internal/config"
	"kevent/gateway/internal/handler"
	"kevent/gateway/internal/kafka"
	"kevent/gateway/internal/llmproxy"
	"kevent/gateway/internal/llmproxy/provider"
	gmetrics "kevent/gateway/internal/metrics"
	"kevent/gateway/internal/ratelimit"
	"kevent/gateway/internal/service"
	"kevent/gateway/internal/storage"
)

// version is set at build time via -ldflags "-X main.version=v0.4.1".
var version = "dev"

// routerHolder is an atomically-swappable http.Handler.
// The outer http.Server always points to this wrapper; hot reload replaces the inner router.
type routerHolder struct {
	p atomic.Pointer[chi.Mux]
}

func (h *routerHolder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.p.Load().ServeHTTP(w, r)
}

// reservedGatewayPaths lists the path prefixes owned by the gateway itself.
// Configured service paths matching any of these are silently skipped to
// prevent accidental overrides of built-in routes.
var reservedGatewayPaths = []string{
	"/health",
	"/metrics",
	"/docs",
	"/openapi.yaml",
	"/jobs",
	"/-",
}

// reservedGatewayPath reports whether path starts with a reserved gateway prefix.
func reservedGatewayPath(path string) bool {
	for _, prefix := range reservedGatewayPaths {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func buildRouter(
	cfg *config.Config,
	reg *service.Registry,
	s3Client *storage.S3Client,
	redisClient *storage.RedisClient,
	producer *kafka.Producer,
	logger *slog.Logger,
	reloadFn func() error,
	rl ratelimit.Checker,
	llmHandler *llmproxy.Handler,
) *chi.Mux {
	jobHandler := handler.NewJobHandler(reg, s3Client, redisClient, producer, cfg.Server.PriorityHeader, cfg.Server.ConsumerHeader, rl)

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(handler.StructuredLogger(logger))
	r.Use(chimw.Recoverer)

	spec := handler.GenerateSpec(reg, version)
	swaggerSpecs := handler.FetchSwaggerSpecs(cfg.Services)

	r.Get("/health", handler.Health)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	r.Get("/docs", handler.DocsUI(swaggerSpecs))
	r.Get("/openapi.yaml", handler.NewDocsSpec(spec))
	r.Get("/docs/spec/{type}/{model}", handler.NewSwaggerHandler(swaggerSpecs))
	r.Get("/jobs", jobHandler.ListJobs)
	r.Post("/jobs/{service_type}", jobHandler.Submit)
	r.Get("/jobs/{service_type}/{id}", jobHandler.GetStatus)
	r.Post("/-/reload", handler.NewReloadHandler(reloadFn))

	if reg.HasSyncServices() {
		syncHandler := handler.NewSyncHandler(reg, s3Client, redisClient, producer, cfg.Server.ConsumerHeader, rl, llmHandler)
		r.Get("/v1/models", handler.ListModels(reg))
		// Register each configured path exactly. Chi handles {model} parameter
		// patterns natively. Single-segment paths (e.g. /rerank) are reachable
		// without needing a separate wildcard route.
		// Reserved gateway paths are skipped — they cannot be overridden by config.
		for _, path := range reg.SyncPaths() {
			if reservedGatewayPath(path) {
				slog.Warn("skipping sync path: conflicts with reserved gateway route", "path", path)
				continue
			}
			r.Post(path, syncHandler.ServeHTTP)
		}
		slog.Info("sync proxy enabled", "paths", reg.SyncPaths())
	}

	return r
}

func main() {
	// JSON structured logger — compatible with log aggregators (Loki, Datadog, …).
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// ── Config ────────────────────────────────────────────────────────────────
	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		cfgPath = v
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// ── Service registry ──────────────────────────────────────────────────────
	initialRegistry := service.NewRegistry(cfg.Services)
	slog.Info("service registry initialised", "types", initialRegistry.Types())

	// ── Dependencies ──────────────────────────────────────────────────────────
	s3Client, err := storage.NewS3Client(cfg.S3, cfg.Encryption)
	if err != nil {
		slog.Error("failed to initialise S3 storage", "error", err)
		os.Exit(1)
	}
	slog.Info("S3 storage initialised", "encryption", cfg.Encryption.Key != "")

	redisClient, err := storage.NewRedis(cfg.Redis)
	if err != nil {
		slog.Error("failed to initialise Redis", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()

	var rl ratelimit.Checker
	if len(cfg.RateLimits) > 0 {
		rl = ratelimit.New(redisClient.Client(), cfg.RateLimits, cfg.Server.ConsumerHeader, cfg.Server.UserTypeHeader)
		slog.Info("rate limiting enabled", "services", len(cfg.RateLimits))
	}

	// Kafka producer and consumer manager are created whenever brokers are
	// configured, regardless of the initial service count. This allows hot
	// reload to add or remove Kafka services without restarting the pod.
	var producer *kafka.Producer
	var consumerManager *kafka.ConsumerManager
	if len(cfg.Kafka.Brokers) > 0 {
		producer, err = kafka.NewProducer(cfg.Kafka)
		if err != nil {
			slog.Error("failed to initialise Kafka producer", "error", err)
			os.Exit(1)
		}
		defer producer.Close()

		consumerManager, err = kafka.NewConsumerManager(cfg.Kafka, redisClient, s3Client, logger)
		if err != nil {
			slog.Error("failed to initialise Kafka consumer manager", "error", err)
			os.Exit(1)
		}
		slog.Info("Kafka initialised", "brokers", cfg.Kafka.Brokers)
	}

	// ── LLM proxy ─────────────────────────────────────────────────────────────
	providerRegistry := provider.NewRegistry()
	responseCache := cache.NewRedisCache(redisClient.Raw())

	var consumerTracker gmetrics.ConsumerTracker = gmetrics.NoopTracker{}
	if cfg.Metrics.TopConsumers > 0 {
		consumerTracker = gmetrics.NewRedisTracker(redisClient.Raw())
	}

	var semCache *semantic.Cache
	if cfg.SemanticCache.EmbeddingServiceURL != "" {
		threshold := cfg.SemanticCache.Threshold
		if threshold == 0 {
			threshold = 0.95
		}
		dim := cfg.SemanticCache.EmbeddingDim
		if dim == 0 {
			dim = 1024
		}
		embedder := semantic.NewHTTPEmbedder(
			cfg.SemanticCache.EmbeddingServiceURL,
			cfg.SemanticCache.EmbeddingModel,
			nil,
		)
		semCache = semantic.New(redisClient.Raw(), embedder, threshold, dim)
		slog.Info("semantic cache enabled",
			"service", cfg.SemanticCache.EmbeddingServiceURL,
			"model", cfg.SemanticCache.EmbeddingModel,
			"threshold", threshold,
		)
	}

	llmHandler := llmproxy.New(responseCache, providerRegistry, &http.Client{Timeout: 15 * time.Minute},
		cfg.Server.UserTypeHeader, consumerTracker, semCache)

	// ── Hot-reload ────────────────────────────────────────────────────────────
	// reloadFn re-reads the config file, atomically swaps the active router,
	// and reconciles Kafka consumers (stopping removed, starting added topics).
	// Infrastructure (S3, Redis, Kafka connection) is not re-initialised.
	holder := &routerHolder{}

	var reloadFn func() error
	reloadFn = func() error {
		newCfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		newReg := service.NewRegistry(newCfg.Services)
		newRouter := buildRouter(newCfg, newReg, s3Client, redisClient, producer, logger, reloadFn, rl, llmHandler)
		holder.p.Store(newRouter)
		if consumerManager != nil {
			consumerManager.Reconcile(newReg)
		}
		slog.Info("service registry reloaded", "types", newReg.Types())
		return nil
	}

	// ── HTTP router ───────────────────────────────────────────────────────────
	initialRouter := buildRouter(cfg, initialRegistry, s3Client, redisClient, producer, logger, reloadFn, rl, llmHandler)
	holder.p.Store(initialRouter)

	// ── Result consumers ──────────────────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.Metrics.TopConsumers > 0 {
		gmetrics.StartTopNRefresh(ctx, redisClient.Raw(), cfg.Metrics.TopConsumers, 60*time.Second)
	}

	if consumerManager != nil {
		consumerManager.Start(ctx, initialRegistry)
	}

	// ── HTTP server ───────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      holder,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server starting", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server error", "error", err)
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	}

	slog.Info("shutting down…")
	cancel() // stop all Kafka consumers

	if consumerManager != nil {
		consumerManager.Wait() // drain in-flight consumers and webhooks
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}

	slog.Info("server stopped")
}
