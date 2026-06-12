package config_test

import (
	"os"
	"testing"
	"time"

	"gatewai/gateway/internal/config"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("creating temp config: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	f.Close()
	return f.Name()
}

// minimalValid returns a config that satisfies all validation rules with the
// minimal set of required fields.
const minimalValid = `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    inference_url: "http://inference.svc"
`

// syncOnlyValid has a service with only a direct proxy backend (no async file upload).
const syncOnlyValid = `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    model: whisper
    inference_url: "http://whisper.svc"
`

// ── Load errors ───────────────────────────────────────────────────────────────

func TestLoad_FileNotFound(t *testing.T) {
	_, err := config.Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeConfig(t, "not: valid: yaml: {{{")
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

// ── Defaults ──────────────────────────────────────────────────────────────────

func TestLoad_Defaults(t *testing.T) {
	path := writeConfig(t, minimalValid)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.Addr != ":8080" {
		t.Errorf("Addr: expected :8080, got %q", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 120*time.Second {
		t.Errorf("ReadTimeout: expected 120s, got %v", cfg.Server.ReadTimeout)
	}
	if cfg.Server.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout: expected 120s, got %v", cfg.Server.IdleTimeout)
	}
	if cfg.Lifecycle.JobTTL.GlobalDuration() != 0 {
		t.Errorf("lifecycle.job_ttl.global: expected 0 (unset), got %v", cfg.Lifecycle.JobTTL.GlobalDuration())
	}
	if cfg.Lifecycle.PersistsResult {
		t.Errorf("lifecycle.persists_result: expected false (default), got true")
	}
	for i, svc := range cfg.Services {
		if svc.MaxFileSizeMB != 100 {
			t.Errorf("services[%d].MaxFileSizeMB: expected 100, got %d", i, svc.MaxFileSizeMB)
		}
	}
}

func TestLoad_DefaultsNotOverrideExplicit(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
lifecycle:
  persists_result: true
  job_ttl:
    global: "48h"
server:
  addr: ":9090"
  read_timeout: 30s
services:
  - type: transcription
    inference_url: "http://inference.svc"
    max_file_size_mb: 200
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("explicit addr should not be overridden, got %q", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 30*time.Second {
		t.Errorf("explicit read_timeout should not be overridden, got %v", cfg.Server.ReadTimeout)
	}
	if cfg.Lifecycle.JobTTL.GlobalDuration() != 48*time.Hour {
		t.Errorf("explicit lifecycle.job_ttl.global should not be overridden, got %v", cfg.Lifecycle.JobTTL.GlobalDuration())
	}
	if !cfg.Lifecycle.PersistsResult {
		t.Errorf("explicit lifecycle.persists_result should not be overridden, got false")
	}
	if cfg.Services[0].MaxFileSizeMB != 200 {
		t.Errorf("explicit max_file_size_mb should not be overridden, got %d", cfg.Services[0].MaxFileSizeMB)
	}
}

// ── Env expansion ─────────────────────────────────────────────────────────────

func TestLoad_EnvVar_Set(t *testing.T) {
	t.Setenv("TEST_BUCKET", "env-bucket")
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: ${TEST_BUCKET}
redis:
  addr: "localhost:6379"
services:
  - type: t
    inference_url: "http://svc"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.S3.Bucket != "env-bucket" {
		t.Errorf("expected bucket from env, got %q", cfg.S3.Bucket)
	}
}

func TestLoad_EnvVar_DefaultUsed(t *testing.T) {
	os.Unsetenv("UNSET_KEVENT_VAR")
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: ${UNSET_KEVENT_VAR:-fallback-bucket}
redis:
  addr: "localhost:6379"
services:
  - type: t
    inference_url: "http://svc"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.S3.Bucket != "fallback-bucket" {
		t.Errorf("expected fallback value, got %q", cfg.S3.Bucket)
	}
}

func TestLoad_EnvVar_DefaultNotUsedWhenSet(t *testing.T) {
	t.Setenv("KEVENT_TEST_VAR", "actual-value")
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: ${KEVENT_TEST_VAR:-fallback}
redis:
  addr: "localhost:6379"
services:
  - type: t
    inference_url: "http://svc"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.S3.Bucket != "actual-value" {
		t.Errorf("expected env value to take precedence, got %q", cfg.S3.Bucket)
	}
}

// ── Validation ────────────────────────────────────────────────────────────────

func TestLoad_Validate_MissingS3Endpoint(t *testing.T) {
	path := writeConfig(t, `
s3:
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: t
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for missing s3.endpoint")
	}
}

func TestLoad_Validate_MissingS3Region(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: t
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for missing s3.region")
	}
}

func TestLoad_Validate_MissingS3Bucket(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
redis:
  addr: "localhost:6379"
services:
  - type: t
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for missing s3.bucket")
	}
}

func TestLoad_Validate_MissingRedisAddr(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
services:
  - type: t
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for missing redis.addr")
	}
}

func TestLoad_Validate_NoServices(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services: []
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for empty services list")
	}
}

func TestLoad_Validate_ServiceEmptyType(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - model: whisper
    inference_url: "http://svc"
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for service with empty type")
	}
}

func TestLoad_Validate_SyncOnlyService(t *testing.T) {
	path := writeConfig(t, syncOnlyValid)
	_, err := config.Load(path)
	if err != nil {
		t.Errorf("sync-only service should load successfully, got: %v", err)
	}
}

func TestLoad_TokenRateWithoutPeriod_Error(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: llm
    model: test
rate_limits:
  llm:
    user:
      token_rate: 100000
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error when token_rate is set without token_period")
	}
}

func TestLoad_TokenRateWithPeriod_OK(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: llm
    model: test
rate_limits:
  llm:
    user:
      token_rate: 100000
      token_period: 1h
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RateLimits["llm"]["user"].TokenRate != 100000 {
		t.Errorf("TokenRate: expected 100000, got %d", cfg.RateLimits["llm"]["user"].TokenRate)
	}
	if cfg.RateLimits["llm"]["user"].TokenPeriod != "1h" {
		t.Errorf("TokenPeriod: expected 1h, got %q", cfg.RateLimits["llm"]["user"].TokenPeriod)
	}
}

func TestLoad_ServiceTokenLimits_MissingPeriod_Error(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: llm
    model: gpt-4o
    token_limits:
      "*":
        token_rate: 10000
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error when service token_rate is set without token_period")
	}
}

func TestLoad_ServiceTokenLimits_OK(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: llm
    model: gpt-4o
    token_limits:
      premium:
        token_rate: 100000
        token_period: 1h
      "*":
        token_rate: 20000
        token_period: 1h
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Services[0].TokenLimits["premium"].TokenRate != 100000 {
		t.Errorf("expected TokenRate=100000 for premium, got %d", cfg.Services[0].TokenLimits["premium"].TokenRate)
	}
	if cfg.Services[0].TokenLimits["*"].TokenPeriod != "1h" {
		t.Errorf("expected TokenPeriod=1h for *, got %q", cfg.Services[0].TokenLimits["*"].TokenPeriod)
	}
}
