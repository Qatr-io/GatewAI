package storage_test

import (
	"context"
	"testing"
	"time"

	"gatewai/gateway/internal/model"
)

func TestSaveJobPushesToRelayQueue(t *testing.T) {
	c, mr := newTestRedisClient(t)
	_ = mr

	job := &model.Job{
		ID:           "job-1",
		Model:        "whisper-diarization",
		ServiceType:  "audio",
		Status:       model.JobStatusPending,
		InputRef:     "job-1/input.wav",
		InferenceURL: "/v1/audio/transcriptions",
		Params:       map[string]string{"language": "fr"},
		CreatedAt:    time.Now(),
	}
	err := c.SaveJob(context.Background(), job)
	if err != nil {
		t.Fatalf("SaveJob returned unexpected error: %v", err)
	}

	rdb := c.Client()
	val, err := rdb.LRange(context.Background(), "relay:whisper-diarization:pending", 0, -1).Result()
	if err != nil {
		t.Fatalf("LRange returned unexpected error: %v", err)
	}
	if len(val) != 1 || val[0] != "job-1" {
		t.Errorf("expected relay queue to contain [job-1], got %v", val)
	}
}
