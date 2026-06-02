package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"kevent/relay/internal/adapter"
	"kevent/relay/internal/config"
	"kevent/relay/internal/kafka"
	"kevent/relay/internal/lifecycle"
	"kevent/relay/internal/relay"
	"kevent/relay/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
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

	s3Client, err := storage.NewS3Client(cfg.S3, cfg.Encryption)
	if err != nil {
		slog.Error("failed to initialise S3 client", "error", err)
		os.Exit(1)
	}

	publisher, err := kafka.NewPublisher(cfg.Kafka)
	if err != nil {
		slog.Error("failed to initialise Kafka publisher", "error", err)
		os.Exit(1)
	}
	defer publisher.Close()

	consumer, err := kafka.NewConsumer(cfg.Kafka)
	if err != nil {
		slog.Error("failed to initialise Kafka consumer", "error", err)
		os.Exit(1)
	}
	defer consumer.Close()

	adp, err := adapter.New(cfg)
	if err != nil {
		slog.Error("failed to initialise adapter", "error", err)
		os.Exit(1)
	}

	annotator := lifecycle.New()
	proc := relay.New(adp, s3Client, publisher, cfg.Service.ResultTopic, annotator)

	inferenceHealthURL := strings.TrimRight(cfg.Inference.BaseURL, "/") + "/health"
	healthClient := &http.Client{Timeout: cfg.Inference.HealthCheckTimeoutDuration()}
	waitForInference(inferenceHealthURL, healthClient, cfg.Inference.ReadyTimeoutDuration(), cfg.Inference.ReadyIntervalDuration())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go serveHealth()

	slog.Info("relay consumer started",
		"topic", cfg.Kafka.InputTopic,
		"consumer_group", cfg.Kafka.ConsumerGroup,
	)

	for {
		msg, err := consumer.ReadMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Info("relay shutting down")
			} else {
				slog.Error("kafka fetch error", "error", err)
			}
			break
		}

		if err := handleMessage(proc, publisher, cfg.Kafka.DLQTopic, msg); err != nil {
			slog.Error("fatal infra error, exiting", "error", err)
			os.Exit(1)
		}

		// After completing a job, honour a pending shutdown signal.
		if ctx.Err() != nil {
			slog.Info("relay shutting down after job completion")
			break
		}
	}
}

// handleMessage processes one Kafka message whose offset is already committed
// (ReadMessage auto-commits before returning). Infra errors are routed to the
// DLQ so the job can be retried without blocking the main consumer loop.
// Returns an error only when DLQ publishing also fails — caller exits the pod.
func handleMessage(proc *relay.Processor, publisher *kafka.Publisher, dlqTopic string, msg kafkago.Message) error {
	event, err := relay.ParseInputEvent(msg.Value)
	if err != nil || event.JobID == "" {
		slog.Error("skipping unparseable message", "error", err, "offset", msg.Offset, "partition", msg.Partition)
		return nil // offset already committed, nothing to recover
	}

	slog.Info("processing job", "job_id", event.JobID, "service_type", event.ServiceType, "offset", msg.Offset)

	if err := proc.Process(context.Background(), event); err != nil {
		// Offset is already committed — we cannot let Kafka redeliver the message.
		// Route to DLQ for retry; exit so Kubernetes restarts the pod.
		slog.Error("infra error after commit, routing to DLQ", "job_id", event.JobID, "error", err)
		if dlqTopic != "" {
			if dlqErr := publisher.PublishRaw(context.Background(), dlqTopic, event.JobID, msg.Value); dlqErr != nil {
				slog.Error("DLQ publish failed, job may be lost", "job_id", event.JobID, "error", dlqErr)
				return fmt.Errorf("infra error %w; DLQ publish: %w", err, dlqErr)
			}
			slog.Warn("job sent to DLQ", "job_id", event.JobID, "dlq_topic", dlqTopic)
		}
		return err // caller will os.Exit(1) — pod restarts, DLQ holds the job
	}

	slog.Info("job completed", "job_id", event.JobID)
	return nil
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
		resp, err := client.Get(healthURL)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
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
