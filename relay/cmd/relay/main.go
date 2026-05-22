package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"kevent/relay/internal/adapter"
	"kevent/relay/internal/config"
	"kevent/relay/internal/kafka"
	"kevent/relay/internal/lifecycle"
	"kevent/relay/internal/metrics"
	"kevent/relay/internal/relay"
	"kevent/relay/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		cfgPath = v
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	slog.Info("sidecar starting",
		"result_topic", cfg.Service.ResultTopic,
		"inference_base_url", cfg.Inference.BaseURL,
	)

	s3Client, err := storage.NewS3Client(cfg.S3, cfg.Encryption)
	if err != nil {
		slog.Error("failed to initialise S3 client", "error", err)
		os.Exit(1)
	}
	slog.Info("S3 storage initialised", "encryption", cfg.Encryption.Key != "")

	publisher, err := kafka.NewPublisher(cfg.Kafka)
	if err != nil {
		slog.Error("failed to initialise Kafka publisher", "error", err)
		os.Exit(1)
	}
	defer publisher.Close()

	adp, err := adapter.New(cfg)
	if err != nil {
		slog.Error("failed to initialise adapter", "error", err)
		os.Exit(1)
	}

	annotator := lifecycle.New()
	if annotator != nil {
		if err := annotator.SetDeletionCost(context.Background(), lifecycle.CostIdle); err != nil {
			slog.Warn("failed to initialise pod deletion cost annotation", "error", err)
		}
	}
	disp := relay.New(adp, s3Client, publisher, cfg.Service.ResultTopic, annotator)

	inferenceHealthURL := strings.TrimRight(cfg.Inference.BaseURL, "/") + "/health"
	healthClient := &http.Client{Timeout: cfg.Inference.HealthCheckTimeoutDuration()}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		resp, err := healthClient.Get(inferenceHealthURL)
		if err != nil {
			http.Error(w, "inference not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf("inference not ready: status %d", resp.StatusCode), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// /sync — priority CloudEvent handler for sync-over-Kafka jobs.
	// Sets syncPriority flag so async jobs on the same pod are deferred (503).
	mux.HandleFunc("/sync", disp.ServeHTTPSync)

	// / — async CloudEvent handler for KafkaSource (always POSTs to exactly "/").
	// Any other path (e.g. /v1/audio/transcriptions) is a direct inference request
	// from the gateway sync-direct path; proxy it straight to the local model.
	inferenceProxy := newInferenceProxy(cfg.Inference.BaseURL)
	// Derive service type from result_topic ("jobs.<type>.results") so no
	// additional SERVICE_TYPE env var is required in the ServingRuntime.
	serviceType := cfg.Service.Type()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			inferenceProxy.ServeHTTP(rec, r)
			metrics.ProxyRequestsTotal.WithLabelValues(serviceType, fmt.Sprintf("%d", rec.status)).Inc()
			metrics.ProxyDuration.WithLabelValues(serviceType).Observe(time.Since(start).Seconds())
			return
		}
		disp.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:        ":8080",
		Handler:     mux,
		ReadTimeout: cfg.Server.ReadTimeoutDuration(),
		// WriteTimeout désactivé : le handler bloque pendant toute la durée de
		// l'inférence. Le timeout est géré par Knative via
		// spec.template.spec.timeoutSeconds sur le Service.
		WriteTimeout: 0,
		IdleTimeout:  cfg.Server.IdleTimeoutDuration(),
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server error", "error", err)
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	}

	slog.Info("shutting down…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeoutDuration())
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}

	slog.Info("server stopped")
}

// newInferenceProxy returns a reverse proxy that forwards requests to the local
// inference model (base_url). Used for sync-direct requests that arrive on paths
// other than "/" (which is reserved for async CloudEvents from KafkaSource).
func newInferenceProxy(baseURL string) *httputil.ReverseProxy {
	target, err := url.Parse(baseURL)
	if err != nil || target.Host == "" {
		slog.Error("invalid inference base_url — cannot start proxy", "url", baseURL, "error", err)
		os.Exit(1)
	}
	return httputil.NewSingleHostReverseProxy(target)
}

// statusRecorder wraps http.ResponseWriter to capture the written status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
