package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestValidate_MissingRedisAddr(t *testing.T) {
	cfg := &Config{
		Model: "whisper-large-v3",
		S3: S3Config{
			Endpoint: "https://s3.example.com",
			Region:   "fr-par",
			Bucket:   "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "redis.addr") {
		t.Errorf("expected redis.addr validation error, got %v", err)
	}
}

func TestValidate_MissingModel(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{Addr: "redis:6379"},
		S3: S3Config{
			Endpoint: "https://s3.example.com",
			Region:   "fr-par",
			Bucket:   "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("expected model validation error, got %v", err)
	}
}

func TestValidate_MissingS3Endpoint(t *testing.T) {
	cfg := &Config{
		Model: "whisper-large-v3",
		Redis: RedisConfig{Addr: "redis:6379"},
		S3: S3Config{
			Region: "fr-par",
			Bucket: "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "s3.endpoint") {
		t.Errorf("expected s3.endpoint validation error, got %v", err)
	}
}

func TestTimeoutDuration(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"300s", 300 * time.Second},
		{"5m", 5 * time.Minute},
		{"0s", 0},
		{"0", 0},
		{"", 300 * time.Second},
		{"invalid", 300 * time.Second},
		{"-1s", 0},
	}

	for _, tc := range cases {
		got := (InferenceConfig{Timeout: tc.input}).TimeoutDuration()
		if got != tc.want {
			t.Errorf("TimeoutDuration(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestUsageConfig_RetentionDuration_Default(t *testing.T) {
	var u UsageConfig
	if got := u.RetentionDuration(); got != 0 {
		t.Errorf("empty retention: got %v, want 0", got)
	}
}

func TestUsageConfig_RetentionDuration_Parses(t *testing.T) {
	u := UsageConfig{Retention: "720h"}
	if got := u.RetentionDuration(); got != 720*time.Hour {
		t.Errorf("got %v, want 720h", got)
	}
}

func TestLoad_ParsesRateLimitsAndAccountingConfig(t *testing.T) {
	yamlContent := `
model: "whisper-large-v3"
redis:
  addr: "localhost:6379"
s3:
  endpoint: "https://s3.example.com"
  region: "fr-par"
  bucket: "bucket"
inference:
  base_url: "http://127.0.0.1:9000"
rate_limits:
  transcription:
    user:
      token_rate: 100000
      token_period: "24h"
      processing_time: 3600
      processing_period: "24h"
usage:
  retention: "8760h"
persists_result: true
`
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rl := cfg.RateLimits["transcription"]["user"]
	if rl.TokenRate != 100000 || rl.ProcessingTime != 3600 {
		t.Errorf("got %+v, want token_rate=100000 processing_time=3600", rl)
	}
	if cfg.Usage.RetentionDuration() != 8760*time.Hour {
		t.Errorf("usage retention: got %v, want 8760h", cfg.Usage.RetentionDuration())
	}
	if !cfg.PersistsResult {
		t.Error("expected persists_result=true")
	}
}
