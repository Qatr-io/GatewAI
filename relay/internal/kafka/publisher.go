package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	kafkago "github.com/segmentio/kafka-go"

	"kevent/relay/internal/config"
	"kevent/relay/internal/metrics"
	"kevent/relay/internal/model"
)

// Publisher manages one kafka-go Writer per topic, created lazily on first use.
// Writers are thread-safe; the internal map is protected by a mutex.
type Publisher struct {
	brokers   []string
	transport *kafkago.Transport
	mu        sync.Mutex
	writers   map[string]*kafkago.Writer
}

func NewPublisher(cfg config.KafkaConfig) (*Publisher, error) {
	transport, err := buildTransport(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kafka transport: %w", err)
	}
	return &Publisher{
		brokers:   cfg.Brokers,
		transport: transport,
		writers:   make(map[string]*kafkago.Writer),
	}, nil
}

// writerFor returns an existing writer or creates a new one for the topic.
func (p *Publisher) writerFor(topic string) *kafkago.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()

	if w, ok := p.writers[topic]; ok {
		return w
	}

	w := &kafkago.Writer{
		Addr:  kafkago.TCP(p.brokers...),
		Topic: topic,
		// Hash balancer routes messages with the same job_id to the same partition.
		Balancer:               &kafkago.Hash{},
		RequiredAcks:           kafkago.RequireOne,
		AllowAutoTopicCreation: false,
	}
	if p.transport != nil {
		w.Transport = p.transport
	}
	p.writers[topic] = w
	return w
}

// PublishResultEvent serialises the event and writes it to the result topic.
// The job_id is used as the Kafka message key.
func (p *Publisher) PublishResultEvent(ctx context.Context, topic string, event *model.ResultEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshaling result event: %w", err)
	}

	if err := p.writerFor(topic).WriteMessages(ctx, kafkago.Message{
		Key:   []byte(event.JobID),
		Value: payload,
	}); err != nil {
		metrics.KafkaPublishErrorsTotal.Inc()
		return fmt.Errorf("writing to topic %q: %w", topic, err)
	}
	return nil
}

// PublishRaw writes raw bytes to topic with jobID as the message key.
// Used for DLQ publishing — preserves the original InputEvent payload verbatim.
func (p *Publisher) PublishRaw(ctx context.Context, topic, jobID string, payload []byte) error {
	if err := p.writerFor(topic).WriteMessages(ctx, kafkago.Message{
		Key:   []byte(jobID),
		Value: payload,
	}); err != nil {
		metrics.KafkaPublishErrorsTotal.Inc()
		return fmt.Errorf("writing to topic %q: %w", topic, err)
	}
	return nil
}

// Close flushes and closes all active writers.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for topic, w := range p.writers {
		_ = w.Close()
		delete(p.writers, topic)
	}
}
