package kafka

import (
	"context"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"kevent/relay/internal/config"
)

// Consumer wraps a kafka-go Reader with auto-commit semantics (at-most-once).
// ReadMessage commits the offset before returning the message, so no other pod
// can receive the same message after a consumer group rebalance.
// Infra failures that occur after commit are handled via the DLQ.
type Consumer struct {
	reader *kafkago.Reader
}

// NewConsumer creates a Consumer for the topic and group in cfg.
func NewConsumer(cfg config.KafkaConfig) (*Consumer, error) {
	dialer, err := buildDialer(cfg)
	if err != nil {
		return nil, err
	}
	r := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:  cfg.Brokers,
		GroupID:  cfg.ConsumerGroup,
		Topic:    cfg.InputTopic,
		Dialer:   dialer,
		MinBytes: 1,
		MaxBytes: 10 << 20,
		MaxWait:  5 * time.Second,
	})
	return &Consumer{reader: r}, nil
}

// ReadMessage blocks until a message is available, commits its offset
// immediately, then returns it. Guaranteed at-most-once delivery per consumer
// group — no rebalance can cause another pod to re-receive the same message.
func (c *Consumer) ReadMessage(ctx context.Context) (kafkago.Message, error) {
	return c.reader.ReadMessage(ctx)
}

// Close closes the underlying reader and leaves the consumer group.
func (c *Consumer) Close() error {
	return c.reader.Close()
}
