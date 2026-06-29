package pgstore

import (
	"context"
	"log/slog"
)

// EventEmitter is the interface gateway handlers use to record usage events.
// Both implementations satisfy it: AsyncEmitter (real) and NoopEmitter (disabled).
type EventEmitter interface {
	EmitLLM(ctx context.Context, e LLMEvent)
	EmitAsyncJob(ctx context.Context, e AsyncJobEvent)
}

// AsyncEmitter wraps a Store with buffered channels for fire-and-forget writes.
// Events are dropped silently when the channel is full (capacity 4096).
type AsyncEmitter struct {
	store   *Store
	llmCh   chan LLMEvent
	asyncCh chan AsyncJobEvent
	done    chan struct{}
}

// NewAsyncEmitter creates an AsyncEmitter and starts its drain goroutine.
// Call Shutdown to drain and stop cleanly on graceful shutdown.
func NewAsyncEmitter(ctx context.Context, store *Store) *AsyncEmitter {
	e := &AsyncEmitter{
		store:   store,
		llmCh:   make(chan LLMEvent, 4096),
		asyncCh: make(chan AsyncJobEvent, 4096),
		done:    make(chan struct{}),
	}
	go e.drain(ctx)
	return e
}

func (e *AsyncEmitter) drain(ctx context.Context) {
	defer close(e.done)
	for {
		select {
		case ev, ok := <-e.llmCh:
			if !ok {
				return
			}
			if err := e.store.WriteLLMEvent(ctx, ev); err != nil {
				slog.Warn("pgstore: write llm event", "error", err)
			}
		case ev, ok := <-e.asyncCh:
			if !ok {
				return
			}
			if err := e.store.WriteAsyncJobEvent(ctx, ev); err != nil {
				slog.Warn("pgstore: write async job event", "error", err)
			}
		case <-ctx.Done():
			// Drain remaining events before exiting.
			for {
				select {
				case ev := <-e.llmCh:
					_ = e.store.WriteLLMEvent(context.Background(), ev)
				case ev := <-e.asyncCh:
					_ = e.store.WriteAsyncJobEvent(context.Background(), ev)
				default:
					return
				}
			}
		}
	}
}

// EmitLLM enqueues an LLM usage event. Non-blocking; drops if channel is full.
func (e *AsyncEmitter) EmitLLM(_ context.Context, ev LLMEvent) {
	select {
	case e.llmCh <- ev:
	default:
	}
}

// EmitAsyncJob enqueues an async job event. Non-blocking; drops if channel is full.
func (e *AsyncEmitter) EmitAsyncJob(_ context.Context, ev AsyncJobEvent) {
	select {
	case e.asyncCh <- ev:
	default:
	}
}

// Shutdown closes the event channels and waits for the drain goroutine to finish.
func (e *AsyncEmitter) Shutdown() {
	close(e.llmCh)
	close(e.asyncCh)
	<-e.done
}

// NoopEmitter satisfies EventEmitter and discards all events.
// Used when postgres.dsn is empty.
type NoopEmitter struct{}

func (NoopEmitter) EmitLLM(_ context.Context, _ LLMEvent)      {}
func (NoopEmitter) EmitAsyncJob(_ context.Context, _ AsyncJobEvent) {}
