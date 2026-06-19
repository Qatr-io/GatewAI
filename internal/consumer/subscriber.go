package consumer

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Subscriber listens on a Redis pub/sub channel for job completion signals.
// One goroutine is spawned per model via Subscribe; it stops when ctx is cancelled.
type Subscriber struct {
	rdb        *redis.Client
	onComplete func(ctx context.Context, jobID string)
}

// NewSubscriber creates a Subscriber that calls onComplete for every job ID
// received on the jobs:{model}:completed channel.
func NewSubscriber(rdb *redis.Client, onComplete func(ctx context.Context, jobID string)) *Subscriber {
	return &Subscriber{rdb: rdb, onComplete: onComplete}
}

// Subscribe launches a goroutine listening on jobs:{model}:completed.
// Stops when ctx is cancelled.
func (s *Subscriber) Subscribe(ctx context.Context, model string) {
	channel := "jobs:" + model + ":completed"
	go func() {
		pubsub := s.rdb.Subscribe(ctx, channel)
		defer pubsub.Close()
		ch := pubsub.Channel()
		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					return
				}
				slog.Info("job completion received", "job_id", msg.Payload, "model", model)
				_, span := otel.Tracer("gatewai/gateway").Start(ctx, "gateway.consumer.job_completed",
					trace.WithAttributes(
						attribute.String("job_id", msg.Payload),
						attribute.String("model", model),
					))
				s.onComplete(ctx, msg.Payload)
				span.End()
			case <-ctx.Done():
				return
			}
		}
	}()
}
