package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/authz"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/pgstore"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
)

// s3Store is the subset of storage.S3Client used by JobHandler.
type s3Store interface {
	Upload(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	GetObject(ctx context.Context, key string) ([]byte, error)
	DeleteObject(ctx context.Context, key string) error
}

// asyncJobStore is the subset of storage.RedisClient used by JobHandler.
type asyncJobStore interface {
	SaveJob(ctx context.Context, job *model.Job) error
	GetJob(ctx context.Context, id string) (*model.Job, error)
	DeleteJob(ctx context.Context, id string) error
	UpdateJobResult(ctx context.Context, jobID string, status model.JobStatus, resultRef, errMsg string) error
	MarkJobCancelled(ctx context.Context, jobID, modelName string) error
	ListJobsByConsumer(ctx context.Context, consumer string, limit, offset int64) ([]*model.Job, int64, error)
	GetQueuePosition(ctx context.Context, jobID, model string) (int64, bool, error)
	ListStalePendingJobs(ctx context.Context, olderThan time.Duration) ([]*model.Job, error)
}

// reservedJobFields are multipart form fields consumed by the gateway
// and excluded from the params map forwarded to the inference API.
var reservedJobFields = map[string]bool{
	"model": true, "file": true, "callback_url": true, "operation": true,
}

// JobHandler handles job submission and status queries.
type JobHandler struct {
	registry              *service.Registry
	store                 s3Store // reuses the interface defined in sync.go
	redis                 asyncJobStore
	priorityHeader        string            // HTTP header that triggers high-priority routing (e.g. "X-Priority")
	consumerHeader        string            // HTTP header identifying the API consumer (e.g. "X-Consumer-Username")
	rateLimiter           ratelimit.Checker // nil = no rate limiting
	lifecycle             config.LifecycleConfig
	concurrentLimiter     ratelimit.ConcurrentChecker     // nil = no concurrent limit
	processingTimeLimiter ratelimit.ProcessingTimeChecker // nil = no processing time limit
	userTypeHeader        string                          // HTTP header carrying user type (e.g. "X-User-Type")
	authz                 *authz.Engine                   // nil = no enforcement
	emitter               pgstore.EventEmitter            // nil = event writes disabled
}

func NewJobHandler(
	registry *service.Registry,
	store s3Store,
	redis asyncJobStore,
	priorityHeader string,
	consumerHeader string,
	rateLimiter ratelimit.Checker,
	lifecycle config.LifecycleConfig,
) *JobHandler {
	return &JobHandler{
		registry:       registry,
		store:          store,
		redis:          redis,
		priorityHeader: priorityHeader,
		consumerHeader: consumerHeader,
		rateLimiter:    rateLimiter,
		lifecycle:      lifecycle,
	}
}

// WithConcurrentLimiter sets the concurrent job limiter and the user-type header name.
func (h *JobHandler) WithConcurrentLimiter(l ratelimit.ConcurrentChecker, userTypeHeader string) *JobHandler {
	h.concurrentLimiter = l
	h.userTypeHeader = userTypeHeader
	return h
}

// WithProcessingTimeLimiter sets the processing time budget limiter.
func (h *JobHandler) WithProcessingTimeLimiter(l ratelimit.ProcessingTimeChecker) *JobHandler {
	h.processingTimeLimiter = l
	return h
}

// WithAuthz sets the authorization engine. nil disables enforcement (default).
func (h *JobHandler) WithAuthz(e *authz.Engine) *JobHandler {
	h.authz = e
	return h
}

// WithEventEmitter sets the PostgreSQL event emitter. nil disables event writes (default).
func (h *JobHandler) WithEventEmitter(e pgstore.EventEmitter) *JobHandler {
	h.emitter = e
	return h
}

// submitResponse is the 202 body returned after a successful job submission.
type submitResponse struct {
	JobID       string `json:"job_id"`
	ServiceType string `json:"service_type"`
	Model       string `json:"model"`
	Status      string `json:"status"`
}

// statusResponse is the body returned on GET /jobs/{service_type}/{id}.
// CallbackURL and ConsumerName are intentionally excluded from the response.
type statusResponse struct {
	JobID         string          `json:"job_id"`
	ServiceType   string          `json:"service_type"`
	Model         string          `json:"model"`
	Status        model.JobStatus `json:"status"`
	QueuePosition *int64          `json:"queue_position,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"` // inline result payload when completed
	Error         string          `json:"error,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// listJobsResponse is the body returned on GET /jobs.
type listJobsResponse struct {
	Consumer string           `json:"consumer"`
	Total    int64            `json:"total"`
	Limit    int64            `json:"limit"`
	Offset   int64            `json:"offset"`
	Jobs     []*jobSummary    `json:"jobs"`
}

type jobSummary struct {
	JobID         string          `json:"job_id"`
	ServiceType   string          `json:"service_type"`
	Model         string          `json:"model"`
	Status        model.JobStatus `json:"status"`
	QueuePosition *int64          `json:"queue_position,omitempty"`
	Error         string          `json:"error,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// Submit handles POST /jobs/{service_type}.
//
// Form fields:
//
//	model        (optional if only one model for the type) – e.g. "whisper-large-v3"
//	file         (required) – the binary file to process
//	callback_url (optional) – webhook URL notified when the job completes
func (h *JobHandler) Submit(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	serviceType := chi.URLParam(r, "service_type")

	// slotHeld tracks whether CheckConcurrent reserved an in-flight slot that
	// must be released once SaveJob completes (success or failure).
	slotHeld := false
	defer func() {
		if slotHeld && h.concurrentLimiter != nil {
			if err := h.concurrentLimiter.ReleaseSlot(context.Background(), r, serviceType); err != nil {
				slog.Warn("failed to release concurrent slot", "error", err)
			}
		}
	}()

	// Set the body size limit using the maximum across all models for this service
	// type, before ParseMultipartForm. The model field inside the form is not yet
	// readable at this point.
	maxSize, err := h.registry.MaxFileSizeForType(serviceType)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if h.rateLimiter != nil {
		rl, err := h.rateLimiter.Check(r.Context(), r, serviceType)
		if err != nil {
			slog.ErrorContext(r.Context(), "rate limit check failed", "error", err)
			// fail open — don't block requests on rate limiter errors
		} else {
			setRateLimitHeaders(w, rl)
			if !rl.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(rl.ResetAfter.Seconds())))
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSize<<20)

	// Buffer up to 32 MB in memory; the rest spills to a temp file on disk.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}

	// Resolve the specific model def now that the form is parsed.
	def, err := h.registry.RouteAsync(serviceType, r.FormValue("model"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Sync-direct services do not accept file uploads and cannot be used asynchronously.
	if !def.SupportsAsync {
		writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("service %q only supports sync requests (POST /v1/*)", def.Model))
		return
	}

	if h.authz != nil {
		p, _ := auth.FromContext(r.Context())
		if h.authz.Allowed(p, def.Type, def.Model) {
			metrics.AuthzDecisionsTotal.WithLabelValues(def.Type, def.Model, "allow").Inc()
		} else {
			consumer := ""
			if p != nil {
				consumer = p.Consumer
			}
			slog.WarnContext(r.Context(), "access denied by policy", "service_type", def.Type, "model", def.Model, "consumer", consumer)
			metrics.AuthzDecisionsTotal.WithLabelValues(def.Type, def.Model, "deny").Inc()
			writeError(w, http.StatusForbidden, fmt.Sprintf("access to model %q denied", def.Model))
			return
		}
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "field 'file' is required")
		return
	}
	defer file.Close()

	if err := h.registry.ValidateFileDef(def, header.Filename); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate the operation before touching S3 or Redis.
	operation := r.FormValue("operation")
	inferenceURL, err := def.OperationPath(operation)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	callbackURL := r.FormValue("callback_url")
	consumerName := ""
	if h.consumerHeader != "" {
		consumerName = r.Header.Get(h.consumerHeader)
	}
	userType := ""
	if h.userTypeHeader != "" {
		userType = r.Header.Get(h.userTypeHeader)
	}

	if h.processingTimeLimiter != nil {
		pr, err := h.processingTimeLimiter.CheckProcessingTime(r.Context(), r, serviceType)
		if err != nil {
			slog.ErrorContext(r.Context(), "processing time check failed", "error", err)
			// fail open
		} else {
			if pr.Limit > 0 {
				w.Header().Set("X-ProcessingTime-Limit", strconv.Itoa(pr.Limit))
				w.Header().Set("X-ProcessingTime-Remaining", strconv.Itoa(pr.Remaining))
			}
			if !pr.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(pr.ResetAfter.Seconds())))
				metrics.ProcessingTimeChecksTotal.WithLabelValues(serviceType, r.Header.Get(h.userTypeHeader), "denied").Inc()
				writeError(w, http.StatusTooManyRequests, "processing time budget exceeded")
				return
			}
			metrics.ProcessingTimeChecksTotal.WithLabelValues(serviceType, r.Header.Get(h.userTypeHeader), "allowed").Inc()
		}
	}

	if h.concurrentLimiter != nil {
		cr, err := h.concurrentLimiter.CheckConcurrent(r.Context(), r, serviceType)
		if err != nil {
			slog.ErrorContext(r.Context(), "concurrent limit check failed", "error", err)
			// fail open — slot NOT held
		} else {
			if cr.Limit > 0 {
				w.Header().Set("X-Concurrent-Limit", strconv.Itoa(cr.Limit))
				w.Header().Set("X-Concurrent-Remaining", strconv.Itoa(cr.Remaining))
			}
			if !cr.Allowed {
				metrics.ConcurrentJobChecksTotal.WithLabelValues(serviceType, userType, "denied").Inc()
				writeError(w, http.StatusTooManyRequests, "concurrent job limit exceeded")
				return
			}
			slotHeld = true
			metrics.ConcurrentJobChecksTotal.WithLabelValues(serviceType, userType, "allowed").Inc()
		}
	}

	// Collect extra form fields to forward to the inference API.
	// Reserved gateway fields are excluded.
	var params map[string]string
	for k, values := range r.MultipartForm.Value {
		if reservedJobFields[k] || len(values) == 0 {
			continue
		}
		if params == nil {
			params = make(map[string]string)
		}
		params[k] = values[0]
	}

	jobID := uuid.New().String()
	ext := filepath.Ext(header.Filename)
	inputRef := fmt.Sprintf("%s/input%s", jobID, ext) // e.g. "abc123/input.wav"

	// Step 1 — store the file in S3.
	if err := h.store.Upload(r.Context(), inputRef, file, header.Size, header.Header.Get("Content-Type")); err != nil {
		slog.ErrorContext(r.Context(), "s3 upload failed", "job_id", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store file")
		return //nolint:nilerr
	}

	priority := h.priorityHeader != "" && r.Header.Get(h.priorityHeader) != ""
	mode := "async"
	if priority {
		mode = "async-priority"
	}

	// Propagate the current span context into the job so the relay can create
	// a child span even though communication happens via Redis (not HTTP).
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(r.Context(), carrier)

	now := time.Now().UTC()
	job := &model.Job{
		ID:           jobID,
		ServiceType:  serviceType,
		Model:        def.Model,
		Status:       model.JobStatusPending,
		InputRef:     inputRef,
		InferenceURL: inferenceURL,
		Params:       params,
		CallbackURL:  callbackURL,
		ConsumerName: consumerName,
		UserType:     userType,
		Priority:     priority,
		CreatedAt:    now,
		UpdatedAt:    now,
		TraceContext: carrier["traceparent"],
	}

	ctx, span := otel.Tracer("gatewai/gateway").Start(r.Context(), "gateway.job.submit",
		trace.WithAttributes(
			attribute.String("job_id", job.ID),
			attribute.String("service_type", serviceType),
			attribute.String("model", def.Model),
			attribute.String("consumer", consumerName),
		))
	defer func() { span.End() }()

	// Step 2 — persist the job record in Redis (also enqueues to relay:<model>:pending).
	if err := h.redis.SaveJob(ctx, job); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "redis save failed", "job_id", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save job")
		return
	}

	if h.emitter != nil {
		subject := ""
		if p, ok := auth.FromContext(ctx); ok && p != nil {
			subject = p.Subject
		}
		h.emitter.EmitAsyncJob(context.WithoutCancel(ctx), pgstore.AsyncJobEvent{
			OccurredAt:  now,
			EventType:   "async_job_submitted",
			Consumer:    consumerName,
			UserType:    userType,
			Subject:     subject,
			ServiceType: serviceType,
			Model:       def.Model,
			JobID:       jobID,
			JobStatus:   string(model.JobStatusPending),
		})
	}

	slog.InfoContext(ctx, "job submitted",
		"job_id", jobID,
		"service_type", serviceType,
		"model", def.Model,
		"file", header.Filename,
		"mode", mode,
	)

	metrics.RequestsTotal.WithLabelValues(mode, serviceType, def.Model, "202").Inc()
	metrics.ObserveWithExemplar(ctx, metrics.RequestDuration.WithLabelValues(mode, serviceType, def.Model), time.Since(start).Seconds())
	metrics.AsyncJobsSubmittedTotal.WithLabelValues(serviceType, def.Model).Inc()
	if consumerName != "" {
		metrics.JobsByConsumerTotal.WithLabelValues(mode, serviceType, def.Model, consumerName).Inc()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(submitResponse{
		JobID:       jobID,
		ServiceType: serviceType,
		Model:       def.Model,
		Status:      string(model.JobStatusPending),
	})
}

// GetStatus handles GET /jobs/{service_type}/{id}.
// When the job is complete the result is inlined in the response body.
//
// Consumer isolation: if consumer_header is configured and the request carries
// that header, the job's consumer_name must match — otherwise 404 is returned.
// This prevents a consumer from fetching another consumer's job even if they
// somehow obtained its UUID. Deployments without consumer_header skip this check.
func (h *JobHandler) GetStatus(w http.ResponseWriter, r *http.Request) {
	serviceType := chi.URLParam(r, "service_type")
	id := chi.URLParam(r, "id")

	job, err := h.redis.GetJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}

	// Validate that the job belongs to the requested service type.
	if job.ServiceType != serviceType {
		writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}

	// Consumer ownership check (opt-in): only enforced when consumer_header is
	// configured AND the header is present in this request.
	// - Header absent → skip check (admin/internal calls, auth-less deployments).
	// - Header present + mismatch → 404 (don't reveal the job exists for another consumer).
	if h.consumerHeader != "" {
		if requester := r.Header.Get(h.consumerHeader); requester != "" {
			if job.ConsumerName != requester {
				writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
				return
			}
		}
	}

	resp := statusResponse{
		JobID:       job.ID,
		ServiceType: job.ServiceType,
		Model:       job.Model,
		Status:      job.Status,
		Error:       job.Error,
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   job.UpdatedAt,
	}

	if job.Status == model.JobStatusPending {
		if pos, ok, err := h.redis.GetQueuePosition(r.Context(), job.ID, job.Model); err != nil {
			slog.WarnContext(r.Context(), "failed to get queue position", "job_id", id, "error", err)
		} else if ok {
			resp.QueuePosition = &pos
		}
	}

	// Fetch and inline the result payload when the job is completed.
	if job.Status == model.JobStatusCompleted && job.ResultRef != "" {
		data, err := h.store.GetObject(r.Context(), job.ResultRef)
		if err != nil {
			slog.ErrorContext(r.Context(), "result fetch failed", "job_id", id, "error", err)
		} else {
			resp.Result = json.RawMessage(data)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)

	// When persists_result is false, clean up immediately after delivery to
	// minimise storage usage. When true, records persist until their TTL expires.
	if job.Status == model.JobStatusCompleted && !h.lifecycle.PersistsResult {
		go func(resultRef, jobID string) {
			ctx := context.Background()
			if resultRef != "" {
				if err := h.store.DeleteObject(ctx, resultRef); err != nil {
					slog.Error("failed to delete result file", "job_id", jobID, "result_ref", resultRef, "error", err)
				}
			}
			if err := h.redis.DeleteJob(ctx, jobID); err != nil {
				slog.Error("failed to delete job record", "job_id", jobID, "error", err)
			}
		}(job.ResultRef, job.ID)
	}
}

// ListJobs handles GET /jobs.
// Requires consumer_header to be configured. Returns the list of jobs for
// the consumer identified by that header, ordered most-recent-first.
//
// Query params:
//
//	limit  (default 20, max 100)
//	offset (default 0)
func (h *JobHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	if h.consumerHeader == "" {
		writeError(w, http.StatusNotImplemented, "GET /jobs requires consumer_header to be configured")
		return
	}

	consumer := r.Header.Get(h.consumerHeader)
	if consumer == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("header %q is required", h.consumerHeader))
		return
	}

	limit := int64(20)
	offset := int64(0)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			offset = n
		}
	}

	jobs, total, err := h.redis.ListJobsByConsumer(r.Context(), consumer, limit, offset)
	if err != nil {
		slog.ErrorContext(r.Context(), "list jobs failed", "consumer", consumer, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}

	summaries := make([]*jobSummary, len(jobs))
	for i, j := range jobs {
		summaries[i] = &jobSummary{
			JobID:         j.ID,
			ServiceType:   j.ServiceType,
			Model:         j.Model,
			Status:        j.Status,
			QueuePosition: j.QueuePosition,
			Error:         j.Error,
			CreatedAt:     j.CreatedAt,
			UpdatedAt:     j.UpdatedAt,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(listJobsResponse{
		Consumer: consumer,
		Total:    total,
		Limit:    limit,
		Offset:   offset,
		Jobs:     summaries,
	})
}

// Cancel handles DELETE /jobs/{service_type}/{id}.
// Deletes a pending job and its S3 input file.
// Applies the same consumer ownership check as GetStatus.
// Returns 409 if the job is already processing or in a terminal state
// (completed/failed): once the relay has started inference, cancellation
// would leak the S3 result file and leave the relay with a missing job record.
func (h *JobHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	serviceType := chi.URLParam(r, "service_type")
	id := chi.URLParam(r, "id")

	job, err := h.redis.GetJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}

	if job.ServiceType != serviceType {
		writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}

	if h.consumerHeader != "" {
		if requester := r.Header.Get(h.consumerHeader); requester != "" {
			if job.ConsumerName != requester {
				writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
				return
			}
		}
	}

	switch job.Status {
	case model.JobStatusPending, model.JobStatusProcessing:
		if err := h.redis.MarkJobCancelled(r.Context(), id, job.Model); err != nil {
			slog.ErrorContext(r.Context(), "cancel: failed", "job_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to cancel job")
			return
		}
		if job.Status == model.JobStatusProcessing {
			metrics.AsyncJobsCancelledWhileProcessingTotal.WithLabelValues(serviceType, job.Model).Inc()
		}
		metrics.AsyncJobsCancelledTotal.WithLabelValues(serviceType, job.Model).Inc()
		slog.InfoContext(r.Context(), "job cancelled", "job_id", id, "service_type", serviceType, "prior_status", job.Status)
		// Job record kept in Redis with status=cancelled; GC handles S3 + record cleanup.
		w.WriteHeader(http.StatusAccepted)

	default:
		writeError(w, http.StatusConflict, fmt.Sprintf("job %q cannot be cancelled in state %q", id, job.Status))
	}
}

const defaultPurgeLimit = 500

// AdminPurge handles POST /-/jobs/purge.
// Deletes stale pending jobs older than `older_than` (e.g. "2h", "30m").
// Restricted to the /-/ admin namespace; caller is responsible for upstream auth.
//
// Query params:
//
//	older_than (required) – duration string, e.g. "2h", "30m"
//	limit      (optional) – max jobs to delete per call (default 500); use to
//	           avoid hammering Redis/S3 on queues with many stale entries.
//	           Call repeatedly until truncated=false to drain fully.
func (h *JobHandler) AdminPurge(w http.ResponseWriter, r *http.Request) {
	rawDur := r.URL.Query().Get("older_than")
	if rawDur == "" {
		writeError(w, http.StatusBadRequest, "query param 'older_than' is required (e.g. '2h')")
		return
	}
	olderThan, err := time.ParseDuration(rawDur)
	if err != nil || olderThan <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid 'older_than' value %q: use a positive Go duration (e.g. '2h', '30m')", rawDur))
		return
	}

	limit := defaultPurgeLimit
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		if n, err := strconv.Atoi(rawLimit); err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid 'limit' value %q: must be a positive integer", rawLimit))
			return
		} else {
			limit = n
		}
	}

	jobs, err := h.redis.ListStalePendingJobs(r.Context(), olderThan)
	if err != nil {
		slog.ErrorContext(r.Context(), "admin purge: list stale jobs failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list stale jobs")
		return
	}

	truncated := len(jobs) > limit
	if truncated {
		jobs = jobs[:limit]
	}

	purged := 0
	for _, job := range jobs {
		if err := h.redis.DeleteJob(r.Context(), job.ID); err != nil {
			slog.WarnContext(r.Context(), "admin purge: delete job failed", "job_id", job.ID, "error", err)
			continue
		}
		purged++
		metrics.AsyncJobsPurgedTotal.WithLabelValues(job.Model).Inc()
		if job.InputRef != "" {
			inputRef := job.InputRef
			jobID := job.ID
			go func() {
				if err := h.store.DeleteObject(context.Background(), inputRef); err != nil {
					slog.Error("admin purge: failed to delete input file", "job_id", jobID, "input_ref", inputRef, "error", err)
				}
			}()
		}
	}

	slog.InfoContext(r.Context(), "admin purge completed", "older_than", rawDur, "found", len(jobs), "purged", purged, "truncated", truncated)

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{
		"older_than": rawDur,
		"found":      len(jobs),
		"purged":     purged,
		"truncated":  truncated,
	})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]string{"error": msg})
}

// setRateLimitHeaders adds X-RateLimit-* headers to the response.
// When Limit is 0 (unlimited plan) no headers are set.
func setRateLimitHeaders(w http.ResponseWriter, r ratelimit.CheckResult) {
	if r.Limit <= 0 {
		return
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(r.Limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(r.Remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(r.ResetAfter).Unix(), 10))
}
