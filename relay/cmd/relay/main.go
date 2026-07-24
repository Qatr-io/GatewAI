package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"gatewai/relay/internal/adapter"
	"gatewai/relay/internal/config"
	"gatewai/relay/internal/metrics"
	"gatewai/relay/internal/model"
	"gatewai/relay/internal/queue"
	relayproc "gatewai/relay/internal/relay"
	"gatewai/relay/internal/storage"
	"gatewai/relay/internal/store"
	"gatewai/relay/internal/telemetry"
)

// version is set at build time via -ldflags "-X main.version=v0.x.y".
var version = "dev"

// redisPublisher implements relay.eventPublisher via the Redis store and queue.
// For every completed or failed job it:
//  1. Atomically updates the job record in Redis (UpdateJobResult).
//  2. Publishes a completion notification on jobs:{model}:completed.
//  3. Removes the job from the processing list (Done).
//  4. Makes a single bounded HTTP call to the gateway's completion callback,
//     so the gateway can perform the rate-limit debit, usage tracking, and
//     webhook delivery exactly once per job (see internal/handler.RelayCompleteHandler
//     on the gateway side). Best-effort: failures are logged and counted, but
//     never fail the job — the job's own result in Redis is unaffected.
type redisPublisher struct {
	st             *store.Store
	q              *queue.Queue
	gatewayBaseURL string
	httpClient     *http.Client
}

func (p *redisPublisher) PublishResult(ctx context.Context, jobID string, status model.JobStatus, resultRef, errMsg string, processingTime float64, promptTokens, completionTokens int64) error {
	if err := p.st.UpdateJobResult(ctx, jobID, status, resultRef, errMsg, processingTime, promptTokens, completionTokens); err != nil {
		return err
	}
	if err := p.q.Publish(ctx, jobID); err != nil {
		slog.Warn("failed to publish job completion notification", "job_id", jobID, "error", err)
		metrics.RedisPublishErrorsTotal.Inc()
	}
	if err := p.q.Done(ctx, jobID); err != nil {
		slog.Warn("failed to remove job from processing list", "job_id", jobID, "error", err)
		metrics.RedisDoneErrorsTotal.Inc()
	}

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(callCtx, http.MethodPost, p.gatewayBaseURL+"/-/relay/jobs/"+jobID+"/complete", nil)
	if resp, err := p.httpClient.Do(req); err != nil {
		slog.Warn("gateway completion callback failed", "job_id", jobID, "error", err)
		metrics.GatewayCallbackErrorsTotal.Inc()
	} else {
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			slog.Warn("gateway completion callback returned non-2xx", "job_id", jobID, "status", resp.StatusCode)
			metrics.GatewayCallbackErrorsTotal.Inc()
		}
	}

	return nil
}

func main() {
	// Bootstrap logger for config-load phase; reconfigured below once log_level is known.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		cfgPath = v
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()})))

	// ── OpenTelemetry ─────────────────────────────────────────────────────────
	// The relay is a one-shot process: ForceFlush via shutdown is critical so
	// spans are not dropped when the pod exits after processing a single job.
	otelSvcName := cfg.Otel.ServiceName
	if otelSvcName == "" {
		otelSvcName = "gatewai/relay"
	}
	_, otelShutdown, err := telemetry.Setup(context.Background(), cfg.Otel, otelSvcName, version)
	if err != nil {
		slog.Error("failed to initialise OpenTelemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if err := otelShutdown(shutCtx); err != nil {
			slog.Error("OTel shutdown error", "error", err)
		}
	}()

	s3Client, err := storage.NewS3Client(cfg.S3, cfg.Encryption)
	if err != nil {
		slog.Error("failed to initialise S3 client", "error", err)
		os.Exit(1)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		slog.Error("failed to connect to Redis", "addr", cfg.Redis.Addr, "error", err)
		os.Exit(1)
	}

	q := queue.New(rdb, cfg.Model)
	s := store.New(rdb)

	pub := &redisPublisher{st: s, q: q, gatewayBaseURL: cfg.Gateway.BaseURL, httpClient: &http.Client{Timeout: 5 * time.Second}}

	adp, err := adapter.New(cfg)
	if err != nil {
		slog.Error("failed to initialise adapter", "error", err)
		os.Exit(1)
	}

	proc := relayproc.New(adp, s3Client, pub)

	inferenceHealthURL := cfg.Inference.HealthCheckURL()
	healthClient := &http.Client{Timeout: cfg.Inference.HealthCheckTimeoutDuration()}
	waitForInference(inferenceHealthURL, healthClient, cfg.Inference.ReadyTimeoutDuration(), cfg.Inference.ReadyIntervalDuration())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go serveHealth()

	slog.Info("relay started", "model", cfg.Model)

	// Pop blocks until a job is available or the context is cancelled (SIGTERM).
	// One pod = one job: after processing, the pod exits so KEDA creates a fresh
	// pod for the next job rather than reusing this one.
	jobID, err := q.Pop(ctx, cfg.QueuePopTimeoutDuration())
	if errors.Is(err, context.Canceled) {
		slog.Info("relay shutting down (no job received)")
		return
	}
	if errors.Is(err, queue.ErrNoJob) {
		slog.Info("queue empty after timeout (job cancelled before pod started), exiting")
		return
	}
	if err != nil {
		slog.Error("queue pop error", "error", err)
		os.Exit(1)
	}

	// jobCtx is cancelled if the gateway sends a DELETE request for this job.
	// SIGTERM does not cancel it — inference must finish (or be cancelled by the gateway).
	jobCtx, cancelJob := context.WithCancel(context.Background())
	defer cancelJob()

	// Subscribe before reading the job so we don't miss a cancel signal published
	// between the pop and the subscribe (pending→processing race window).
	cancelSub := rdb.Subscribe(context.Background(), "relay:"+cfg.Model+":cancel")
	go func() {
		defer cancelSub.Close()
		for msg := range cancelSub.Channel() {
			if msg.Payload == jobID {
				slog.Info("cancellation signal received from gateway", "job_id", jobID)
				cancelJob()
				return
			}
		}
	}()

	relayTracer := otel.Tracer("gatewai/relay")
	getJobStart := time.Now()
	job, err := s.GetJob(ctx, jobID)
	getJobEnd := time.Now()
	if err != nil {
		// Root span: traceparent unknown, we still want the failure recorded.
		_, getJobSpan := relayTracer.Start(ctx, "relay.redis.get_job",
			trace.WithTimestamp(getJobStart),
			trace.WithAttributes(attribute.String("job_id", jobID)))
		getJobSpan.RecordError(err)
		getJobSpan.SetStatus(codes.Error, err.Error())
		getJobSpan.End(trace.WithTimestamp(getJobEnd))
		slog.Error("failed to get job from Redis", "job_id", jobID, "error", err)
		os.Exit(1)
	}
	// Parent the span under the gateway trace now that we have the traceparent.
	// Use backdated timestamps so the span reflects the actual Redis call timing.
	getJobParentCtx := ctx
	if job.TraceContext != "" {
		carrier := propagation.MapCarrier{"traceparent": job.TraceContext}
		getJobParentCtx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	}
	_, getJobSpan := relayTracer.Start(getJobParentCtx, "relay.redis.get_job",
		trace.WithTimestamp(getJobStart),
		trace.WithAttributes(attribute.String("job_id", jobID)))
	getJobSpan.End(trace.WithTimestamp(getJobEnd))
	slog.Debug("redis job retrieved", "job_id", jobID)

	// If the gateway cancelled the job between our pop and this read, stop now.
	if job.Status == model.JobStatusCancelled {
		slog.Info("job already cancelled, skipping inference", "job_id", jobID)
		if derr := q.Done(context.Background(), jobID); derr != nil {
			slog.Warn("failed to remove cancelled job from processing", "job_id", jobID, "error", derr)
		}
		return
	}

	if err := proc.Process(jobCtx, job); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("job cancelled by gateway, removing from processing", "job_id", jobID)
			if derr := q.Done(context.Background(), jobID); derr != nil {
				slog.Warn("failed to remove cancelled job from processing", "job_id", jobID, "error", derr)
			}
			return
		}
		slog.Error("fatal job error, exiting", "job_id", jobID, "error", err)
		os.Exit(1)
	}
}

func serveHealth() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if err := http.ListenAndServe(":8080", mux); err != nil {
		slog.Error("health server stopped", "error", err)
	}
}

func waitForInference(healthURL string, client *http.Client, timeout, interval time.Duration) {
	slog.Info("waiting for inference service", "health_url", healthURL, "timeout", timeout)
	deadline := time.Now().Add(timeout)
	for {
		resp, err := client.Get(healthURL) //nolint:noctx
		if err == nil {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				slog.Info("inference service ready")
				return
			}
		}
		if time.Now().After(deadline) {
			slog.Error("inference service did not become ready within timeout", "health_url", healthURL)
			os.Exit(1)
		}
		slog.Info("inference not ready yet, retrying", "health_url", healthURL, "interval", interval)
		time.Sleep(interval)
	}
}
