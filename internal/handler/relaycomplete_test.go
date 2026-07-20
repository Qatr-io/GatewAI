package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/storage"
)

func newTestRedis(t *testing.T) (*storage.RedisClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := storage.NewRedis(config.RedisConfig{Addr: mr.Addr()}, config.LifecycleConfig{})
	if err != nil {
		t.Fatalf("failed to create redis client: %v", err)
	}
	return c, mr
}

func seedJob(t *testing.T, mr *miniredis.Miniredis, job *model.Job) {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	mr.Set("job:"+job.ID, string(data))
}

func newCompleteRequest(id string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/-/relay/jobs/"+id+"/complete", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

type stubS3 struct {
	getData []byte
	getErr  error
	deleted []string
}

func (s *stubS3) GetObject(_ context.Context, _ string) ([]byte, error) {
	return s.getData, s.getErr
}

func (s *stubS3) DeleteObject(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	return nil
}

type addProcessingTimeCall struct {
	consumer, userType, serviceType string
	seconds                         float64
}

type stubProcessingTimeLimiter struct {
	addCalls []addProcessingTimeCall
}

func (s *stubProcessingTimeLimiter) CheckProcessingTime(context.Context, *http.Request, string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}

func (s *stubProcessingTimeLimiter) AddProcessingTime(_ context.Context, consumer, userType, serviceType string, seconds float64) error {
	s.addCalls = append(s.addCalls, addProcessingTimeCall{consumer, userType, serviceType, seconds})
	return nil
}

type addTokensForCall struct {
	consumer, userType, serviceType string
	total                           int
}

type stubTokenLimiter struct {
	addForCalls []addTokensForCall
}

func (s *stubTokenLimiter) CheckTokens(context.Context, *http.Request, string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}
func (s *stubTokenLimiter) AddTokens(context.Context, *http.Request, string, int) error { return nil }
func (s *stubTokenLimiter) CheckModelTokens(context.Context, *http.Request, string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}
func (s *stubTokenLimiter) AddModelTokens(context.Context, *http.Request, string, int) error {
	return nil
}
func (s *stubTokenLimiter) AddTokensFor(_ context.Context, consumer, userType, serviceType string, total int) error {
	s.addForCalls = append(s.addForCalls, addTokensForCall{consumer, userType, serviceType, total})
	return nil
}

type trackProcessingTimeCall struct {
	consumer, serviceType string
	seconds               float64
}
type trackTokensCall struct {
	consumer, serviceType string
	prompt, completion    int64
}
type trackUserTypeCall struct {
	consumer, serviceType, userType string
}

type stubUsageTracker struct {
	trackProcessingTimeCalls []trackProcessingTimeCall
	trackTokensCalls         []trackTokensCall
	trackActiveCalls         []string
	trackUserTypeCalls       []trackUserTypeCall
}

func (s *stubUsageTracker) TrackRequest(context.Context, string, string) {}
func (s *stubUsageTracker) TrackJob(context.Context, string, string)     {}
func (s *stubUsageTracker) TrackProcessingTime(_ context.Context, consumer, serviceType string, seconds float64) {
	s.trackProcessingTimeCalls = append(s.trackProcessingTimeCalls, trackProcessingTimeCall{consumer, serviceType, seconds})
}
func (s *stubUsageTracker) TrackTokens(_ context.Context, consumer, serviceType string, prompt, completion int64) {
	s.trackTokensCalls = append(s.trackTokensCalls, trackTokensCall{consumer, serviceType, prompt, completion})
}
func (s *stubUsageTracker) TrackActive(_ context.Context, consumer string) {
	s.trackActiveCalls = append(s.trackActiveCalls, consumer)
}
func (s *stubUsageTracker) TrackUserType(_ context.Context, consumer, serviceType, userType string) {
	s.trackUserTypeCalls = append(s.trackUserTypeCalls, trackUserTypeCall{consumer, serviceType, userType})
}
func (s *stubUsageTracker) UpdateRetention(time.Duration) {}

func TestComplete_JobNotFound_Returns404(t *testing.T) {
	rc, _ := newTestRedis(t)
	h := NewRelayCompleteHandler(rc, &stubS3{}, false)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("missing"))

	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestComplete_DebitsAndTracksUsage(t *testing.T) {
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-1", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusCompleted, ConsumerName: "acme", UserType: "user",
		ProcessingTime: 12.5, PromptTokens: 100, CompletionTokens: 50,
	}
	seedJob(t, mr, job)

	h := NewRelayCompleteHandler(rc, &stubS3{}, false)
	ptLimiter := &stubProcessingTimeLimiter{}
	tokLimiter := &stubTokenLimiter{}
	tracker := &stubUsageTracker{}
	h.WithProcessingTimeLimiter(ptLimiter)
	h.WithTokenLimiter(tokLimiter)
	h.WithUsageTracker(tracker)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if len(ptLimiter.addCalls) != 1 || ptLimiter.addCalls[0].seconds != 12.5 {
		t.Errorf("processing time debit: got %+v", ptLimiter.addCalls)
	}
	if len(tokLimiter.addForCalls) != 1 || tokLimiter.addForCalls[0].total != 150 {
		t.Errorf("token debit: got %+v", tokLimiter.addForCalls)
	}
	if len(tracker.trackProcessingTimeCalls) != 1 {
		t.Errorf("expected TrackProcessingTime call, got %+v", tracker.trackProcessingTimeCalls)
	}
	if len(tracker.trackTokensCalls) != 1 {
		t.Errorf("expected TrackTokens call, got %+v", tracker.trackTokensCalls)
	}
	if len(tracker.trackUserTypeCalls) != 1 || tracker.trackUserTypeCalls[0].userType != "user" {
		t.Errorf("expected TrackUserType call, got %+v", tracker.trackUserTypeCalls)
	}
}

func TestComplete_ZeroProcessingTimeAndTokens_NoDebitOrTrack(t *testing.T) {
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-3", ServiceType: "transcription", Status: model.JobStatusCompleted,
		ConsumerName: "acme", UserType: "user",
	}
	seedJob(t, mr, job)

	h := NewRelayCompleteHandler(rc, &stubS3{}, false)
	ptLimiter := &stubProcessingTimeLimiter{}
	tokLimiter := &stubTokenLimiter{}
	tracker := &stubUsageTracker{}
	h.WithProcessingTimeLimiter(ptLimiter)
	h.WithTokenLimiter(tokLimiter)
	h.WithUsageTracker(tracker)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-3"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if len(ptLimiter.addCalls) != 0 {
		t.Errorf("expected no processing-time debit, got %+v", ptLimiter.addCalls)
	}
	if len(tokLimiter.addForCalls) != 0 {
		t.Errorf("expected no token debit, got %+v", tokLimiter.addForCalls)
	}
	// TrackUserType still fires unconditionally when a consumer is set.
	if len(tracker.trackUserTypeCalls) != 1 {
		t.Errorf("expected TrackUserType call, got %+v", tracker.trackUserTypeCalls)
	}
	if len(tracker.trackProcessingTimeCalls) != 0 || len(tracker.trackTokensCalls) != 0 {
		t.Errorf("expected no TrackProcessingTime/TrackTokens calls, got %+v / %+v", tracker.trackProcessingTimeCalls, tracker.trackTokensCalls)
	}
}

func TestComplete_DispatchesWebhookAndDrainsViaWait(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-2", ServiceType: "transcription", Status: model.JobStatusCompleted,
		CallbackURL: srv.URL, ResultRef: "job-2/result.json",
	}
	seedJob(t, mr, job)

	h := NewRelayCompleteHandler(rc, &stubS3{getData: []byte(`{"text":"hi"}`)}, false)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-2"))
	h.Wait()

	select {
	case <-received:
	default:
		t.Error("expected webhook to be delivered before Wait() returned")
	}
}
