package config

import (
	"testing"
	"time"
)

func TestTimeoutDuration_ParsesAndFallsBackToDefault(t *testing.T) {
	cases := []struct {
		name string
		in   InferenceConfig
		want time.Duration
	}{
		{"explicit valid", InferenceConfig{Timeout: "120s"}, 120 * time.Second},
		{"explicit hours", InferenceConfig{Timeout: "1h"}, time.Hour},
		{"empty falls back", InferenceConfig{}, 300 * time.Second},
		{"unparseable falls back", InferenceConfig{Timeout: "not-a-duration"}, 300 * time.Second},
		{"zero falls back", InferenceConfig{Timeout: "0s"}, 300 * time.Second},
		{"negative falls back", InferenceConfig{Timeout: "-5s"}, 300 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.TimeoutDuration(); got != tc.want {
				t.Fatalf("TimeoutDuration() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTimeoutFor_UsesPerOperationOverride(t *testing.T) {
	cfg := InferenceConfig{
		Timeout: "300s",
		OperationTimeouts: map[string]string{
			"/v1/audio/diarizations":   "3600s",
			"/v1/audio/transcriptions": "20m",
			"/v1/audio/broken":         "not-a-duration",
		},
	}

	cases := []struct {
		name string
		path string
		want time.Duration
	}{
		{
			"diarization uses override",
			"/v1/audio/diarizations", time.Hour,
		},
		{
			"transcription uses override (alt unit)",
			"/v1/audio/transcriptions", 20 * time.Minute,
		},
		{
			"unknown path falls back to default",
			"/v1/embeddings", 300 * time.Second,
		},
		{
			"unparseable override falls back to default",
			"/v1/audio/broken", 300 * time.Second,
		},
		{
			"empty path falls back",
			"", 300 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.TimeoutFor(tc.path); got != tc.want {
				t.Fatalf("TimeoutFor(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestTimeoutFor_NilMapDoesNotPanic(t *testing.T) {
	cfg := InferenceConfig{Timeout: "60s"} // no OperationTimeouts map at all
	if got := cfg.TimeoutFor("/v1/anything"); got != 60*time.Second {
		t.Fatalf("TimeoutFor on nil override map = %v, want 60s", got)
	}
}
