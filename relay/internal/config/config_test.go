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
		Gateway:   GatewayConfig{BaseURL: "http://gatewai-gateway.default.svc.cluster.local"},
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
		Gateway:   GatewayConfig{BaseURL: "http://gatewai-gateway.default.svc.cluster.local"},
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
		Gateway:   GatewayConfig{BaseURL: "http://gatewai-gateway.default.svc.cluster.local"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "s3.endpoint") {
		t.Errorf("expected s3.endpoint validation error, got %v", err)
	}
}

func TestValidate_MissingGatewayBaseURL(t *testing.T) {
	cfg := &Config{
		Model: "whisper-large-v3",
		Redis: RedisConfig{Addr: "redis:6379"},
		S3: S3Config{
			Endpoint: "https://s3.example.com",
			Region:   "fr-par",
			Bucket:   "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "gateway.base_url") {
		t.Errorf("expected gateway.base_url validation error, got %v", err)
	}
}

func TestLoad_ParsesGatewayBaseURL(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	content := `
model: "whisper-large-v3"
redis:
  addr: "redis:6379"
s3:
  endpoint: "https://s3.example.com"
  region: "fr-par"
  bucket: "bucket"
inference:
  base_url: "http://127.0.0.1:9000"
gateway:
  base_url: "http://gatewai-gateway.default.svc.cluster.local"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gateway.BaseURL != "http://gatewai-gateway.default.svc.cluster.local" {
		t.Errorf("Gateway.BaseURL: got %q", cfg.Gateway.BaseURL)
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
