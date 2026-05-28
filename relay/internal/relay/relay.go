package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kevent/relay/internal/adapter"
	"kevent/relay/internal/kafka"
	"kevent/relay/internal/lifecycle"
	"kevent/relay/internal/metrics"
	"kevent/relay/internal/model"
	"kevent/relay/internal/storage"
)

type objectStore interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, int64, string, error)
	PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	DeleteObject(ctx context.Context, key string) error
}

type eventPublisher interface {
	PublishResultEvent(ctx context.Context, topic string, event *model.ResultEvent) error
}

// Dispatcher handles incoming CloudEvent HTTP requests from KafkaSource.
//
// En tant que sidecar, le handler est synchrone : il bloque jusqu'à la fin du
// job et retourne 200 ou 500. Knative Pod Autoscaler mesure la durée de la
// requête HTTP en vol pour calibrer le nombre de replicas (= pods GPU actifs).
// containerConcurrency dans le Knative Service spec contrôle la concurrence max.
type Dispatcher struct {
	adapter      adapter.Adapter
	s3           objectStore
	publisher    eventPublisher
	resultTopic  string
	syncPriority atomic.Int32 // number of sync jobs currently in progress
	activeJobs   atomic.Int32 // number of jobs currently being processed
	wg           sync.WaitGroup
	annotator    *lifecycle.PodAnnotator
}

func New(
	adp adapter.Adapter,
	s3 *storage.S3Client,
	pub *kafka.Publisher,
	resultTopic string,
	annotator *lifecycle.PodAnnotator,
) *Dispatcher {
	return &Dispatcher{
		adapter:     adp,
		s3:          s3,
		publisher:   pub,
		resultTopic: resultTopic,
		annotator:   annotator,
	}
}

// ServeHTTP is the async CloudEvent handler (KafkaSource → POST /).
//
// Returns 503 when a priority sync job is in progress so KafkaSource retries
// after its configured backoffDelay — giving sync jobs the GPU first.
//
// Sémantique des codes retour :
//   - 200 : job traité — pas de retry
//   - 400 : message malformé — pas de retry
//   - 503 : sync job en cours — KafkaSource doit retenter après backoffDelay
//   - 500 : erreur infra transitoire — KafkaSource doit retenter
func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.syncPriority.Load() > 0 {
		slog.Info("async job deferred: sync job in progress")
		metrics.DeferredTotal.Inc()
		http.Error(w, "sync job in progress, retry later", http.StatusServiceUnavailable)
		return
	}
	if d.activeJobs.Load() > 0 {
		slog.Info("async job deferred: job already in progress")
		metrics.DeferredTotal.Inc()
		http.Error(w, "pod busy, retry later", http.StatusServiceUnavailable)
		return
	}
	d.serveHTTP(w, r)
}

// WaitIdle blocks until all in-flight jobs have completed.
// Call after srv.Shutdown to ensure the process does not exit while inference
// or result-publishing is still in progress.
func (d *Dispatcher) WaitIdle() {
	d.wg.Wait()
}

// ActiveJobs returns the number of jobs currently being processed.
func (d *Dispatcher) ActiveJobs() int {
	return int(d.activeJobs.Load())
}

// serveHTTP is the shared CloudEvent processing implementation used by both
// ServeHTTP (async) and ServeHTTPSync (priority).
func (d *Dispatcher) serveHTTP(w http.ResponseWriter, r *http.Request) {
	d.wg.Add(1)
	defer d.wg.Done()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	event, err := decodeInputEvent(r)
	if err != nil {
		slog.Warn("failed to decode input event", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if event.JobID == "" {
		slog.Warn("received event with empty job_id")
		http.Error(w, "missing job_id", http.StatusBadRequest)
		return
	}

	slog.Info("event received", "job_id", event.JobID, "service_type", event.ServiceType, "input_ref", event.InputRef)

	if d.activeJobs.Add(1) == 1 && d.annotator != nil {
		go func() {
			if err := d.annotator.SetDeletionCost(context.Background(), lifecycle.CostBusy); err != nil {
				slog.Warn("failed to set pod deletion cost", "cost", lifecycle.CostBusy, "error", err)
			}
		}()
	}
	defer func() {
		if d.activeJobs.Add(-1) == 0 && d.annotator != nil {
			go func() {
				if err := d.annotator.SetDeletionCost(context.Background(), lifecycle.CostIdle); err != nil {
					slog.Warn("failed to set pod deletion cost", "cost", lifecycle.CostIdle, "error", err)
				}
			}()
		}
	}()

	if err := d.process(r.Context(), event); err != nil {
		slog.Error("transient error, letting KafkaSource retry", "job_id", event.JobID, "error", err)
		http.Error(w, "transient error", http.StatusInternalServerError)
		return
	}

	// Confirmer le succès à KafkaSource AVANT de supprimer le fichier d'entrée.
	// Si la suppression est faite avant, un eviction entre DeleteObject et
	// WriteHeader entraîne un retry KafkaSource sur un fichier déjà disparu (404).
	w.WriteHeader(http.StatusOK)

	go func() {
		if err := d.s3.DeleteObject(context.Background(), event.InputRef); err != nil {
			slog.Error("failed to delete input file", "job_id", event.JobID, "input_ref", event.InputRef, "error", err)
		}
	}()
}

// process orchestre le pipeline complet. Il retourne une erreur uniquement pour
// les pannes infrastructure (S3 indisponible, réseau) afin que KafkaSource
// puisse retenter. Les échecs d'inférence sont publiés en ResultEvent et ne
// génèrent pas d'erreur (le job est définitivement terminé, en échec).
//
// Stratégie de retry : chaque étape transiente (inférence, S3 put result,
// Kafka publish result) est retentée une fois immédiatement avant de
// déléguer à KafkaSource. Le retry inférence implique un nouveau téléchargement
// du fichier depuis S3 (le stream S3 précédent est épuisé).
// Le téléchargement initial (GetObject input) n'est pas retenté : une erreur
// infra S3 à cette étape remonte directement à KafkaSource.
func (d *Dispatcher) process(ctx context.Context, event *model.InputEvent) error {
	log := slog.With("job_id", event.JobID, "service_type", event.ServiceType)
	log.Info("processing job", "input_ref", event.InputRef)

	body, size, contentType, err := d.s3.GetObject(ctx, event.InputRef)
	if err != nil {
		if storage.IsNotFound(err) {
			log.Error("input file not found, publishing permanent failure", "input_ref", event.InputRef)
			metrics.JobsTotal.WithLabelValues(event.ServiceType, "failed").Inc()
			if perr := d.publishFailure(context.Background(), event, "input file not found: "+event.InputRef); perr != nil {
				return fmt.Errorf("publishing not-found failure event: %w", perr)
			}
			return nil
		}
		return fmt.Errorf("s3 get: %w", err)
	}
	defer body.Close()

	if size > 0 {
		metrics.InputSizeBytes.WithLabelValues(event.ServiceType).Observe(float64(size))
	}

	result, inferErr := d.runInference(ctx, event, body, size, contentType)
	if inferErr != nil {
		// Retry inference once immediately: re-download for a fresh stream.
		// Use WithoutCancel so that a queue-proxy timeout that cancelled ctx
		// does not also abort the retry download — inference was already
		// detached by the adapter, so the retry should be too.
		log.Warn("inference attempt failed, retrying immediately", "error", inferErr)
		body2, size2, ct2, getErr := d.s3.GetObject(context.WithoutCancel(ctx), event.InputRef)
		if getErr != nil {
			if storage.IsNotFound(getErr) {
				log.Error("input file not found on inference retry, publishing permanent failure", "input_ref", event.InputRef)
				metrics.JobsTotal.WithLabelValues(event.ServiceType, "failed").Inc()
				if perr := d.publishFailure(context.Background(), event, "input file not found on retry: "+event.InputRef); perr != nil {
					return fmt.Errorf("publishing not-found failure event: %w", perr)
				}
				return nil
			}
			return fmt.Errorf("s3 get on inference retry: %w", getErr)
		}
		defer body2.Close()
		result, inferErr = d.runInference(ctx, event, body2, size2, ct2)
	}

	if inferErr != nil {
		log.Error("inference failed", "error", inferErr)
		metrics.JobsTotal.WithLabelValues(event.ServiceType, "failed").Inc()
		if perr := d.publishFailure(context.Background(), event, fmt.Sprintf("inference: %v", inferErr)); perr != nil {
			return fmt.Errorf("publishing failure event: %w", perr)
		}
		if derr := d.s3.DeleteObject(context.Background(), event.InputRef); derr != nil {
			log.Error("failed to delete input file after failure", "input_ref", event.InputRef, "error", derr)
		}
		return nil
	}

	// Detach from r.Context() for result persistence: the HTTP connection to
	// the queue-proxy may have been closed (proxy timeout, pod drain) while
	// inference was running, cancelling ctx. We must not let that abort the
	// S3 upload or Kafka publish — the result exists and must be persisted.
	persistCtx := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		log.Warn("HTTP context cancelled before result persistence; using detached context", "cause", context.Cause(ctx))
	}

	resultKey := event.JobID + "/result.json"
	if err := d.s3.PutObject(persistCtx, resultKey, bytes.NewReader(result), int64(len(result)), "application/json"); err != nil {
		log.Warn("s3 put attempt failed, retrying immediately", "error", err)
		if err := d.s3.PutObject(persistCtx, resultKey, bytes.NewReader(result), int64(len(result)), "application/json"); err != nil {
			return fmt.Errorf("s3 put: %w", err)
		}
	}

	resultEvent := &model.ResultEvent{
		JobID:       event.JobID,
		ServiceType: event.ServiceType,
		Status:      model.JobStatusCompleted,
		ResultRef:   resultKey,
		CompletedAt: time.Now().UTC(),
	}
	if err := d.publisher.PublishResultEvent(persistCtx, d.resultTopic, resultEvent); err != nil {
		log.Warn("publish result attempt failed, retrying immediately", "error", err)
		if err := d.publisher.PublishResultEvent(persistCtx, d.resultTopic, resultEvent); err != nil {
			log.Error("failed to publish result event after retry", "error", err)
		}
	}

	metrics.JobsTotal.WithLabelValues(event.ServiceType, "completed").Inc()
	log.Info("job completed", "result_ref", resultKey)
	return nil
}

// runInference calls the adapter and records timing metrics.
func (d *Dispatcher) runInference(ctx context.Context, event *model.InputEvent, body io.Reader, size int64, contentType string) ([]byte, error) {
	inferStart := time.Now()
	result, err := d.adapter.Call(ctx, adapter.CallInput{
		JobID:        event.JobID,
		Filename:     filepath.Base(event.InputRef),
		ContentType:  contentType,
		Size:         size,
		Body:         body,
		Model:        event.Model,
		InferenceURL: event.InferenceURL,
		Params:       event.Params,
	})
	metrics.InferenceDuration.WithLabelValues(event.ServiceType).Observe(time.Since(inferStart).Seconds())
	return result, err
}

// decodeInputEvent reads an InputEvent from the HTTP request body.
// It handles both CloudEvent delivery modes:
//   - Binary mode (default): body is the raw Kafka message (JSON InputEvent directly)
//   - Structured mode (Content-Type: application/cloudevents+json): body is a
//     CloudEvent JSON envelope; the InputEvent is nested inside the "data" field.
func decodeInputEvent(r *http.Request) (*model.InputEvent, error) {
	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/cloudevents+json") {
		// Structured CloudEvent — InputEvent is nested inside "data".
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return nil, fmt.Errorf("structured CloudEvent envelope: %w", err)
		}
		payload = envelope.Data
	}

	var event model.InputEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

func (d *Dispatcher) publishFailure(ctx context.Context, event *model.InputEvent, errMsg string) error {
	resultEvent := &model.ResultEvent{
		JobID:       event.JobID,
		ServiceType: event.ServiceType,
		Status:      model.JobStatusFailed,
		Error:       errMsg,
		CompletedAt: time.Now().UTC(),
	}
	if err := d.publisher.PublishResultEvent(ctx, d.resultTopic, resultEvent); err != nil {
		slog.Error("failed to publish failure event", "job_id", event.JobID, "error", err)
		return err
	}
	return nil
}
