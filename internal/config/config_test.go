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

// ── Guardrails config tests ───────────────────────────────────────────────────

func TestLoad_Guardrails_ActionAndChecks_Parses(t *testing.T) {
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
    guardrails:
      action: redact
      checks:
        - pii
        - pii_fr
        - secrets
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g := cfg.Services[0].Guardrails
	if g.Action != "redact" {
		t.Errorf("expected action=redact, got %q", g.Action)
	}
	if len(g.Checks) != 3 {
		t.Errorf("expected 3 checks, got %d", len(g.Checks))
	}
}

func TestLoad_Guardrails_InvalidAction_Error(t *testing.T) {
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
    guardrails:
      action: quarantine
      checks:
        - pii
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for invalid guardrails.action")
	}
}

func TestLoad_Guardrails_InvalidCheck_Error(t *testing.T) {
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
    guardrails:
      action: block
      checks:
        - pii
        - ssn_us
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for unknown guardrails check name")
	}
}

// ── Output guardrails config tests ───────────────────────────────────────────

func TestLoad_Guardrails_Output_Valid(t *testing.T) {
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
    guardrails:
      action: block
      checks:
        - pii
      output:
        action: redact
        checks:
          - pii
          - secrets
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error loading config with output guardrails: %v", err)
	}
	out := cfg.Services[0].Guardrails.Output
	if out == nil {
		t.Fatal("expected output guardrails to be non-nil")
	}
	if out.Action != "redact" {
		t.Errorf("expected output.action=redact, got %q", out.Action)
	}
	if len(out.Checks) != 2 {
		t.Errorf("expected 2 output checks, got %d", len(out.Checks))
	}
}

func TestLoad_Guardrails_Output_InvalidAction_Error(t *testing.T) {
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
    guardrails:
      output:
        action: quarantine
        checks:
          - pii
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for invalid guardrails.output.action")
	}
}

func TestLoad_Guardrails_Output_InvalidCheck_Error(t *testing.T) {
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
    guardrails:
      output:
        action: block
        checks:
          - pii
          - ssn_us
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for unknown guardrails.output check name")
	}
}

// ── Auth config tests ─────────────────────────────────────────────────────────

func TestLoad_Auth_DefaultMode_Legacy(t *testing.T) {
	// No auth block → mode is "" (backward compatible).
	path := writeConfig(t, minimalValid)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.Mode != "" {
		t.Errorf("expected empty auth mode (legacy), got %q", cfg.Auth.Mode)
	}
}

func TestLoad_Auth_InvalidMode_Error(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    inference_url: "http://inference.svc"
auth:
  mode: "basic"
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for invalid auth.mode")
	}
}

func TestLoad_Auth_OAuth2_MissingIssuer_Error(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    inference_url: "http://inference.svc"
auth:
  mode: oauth2
  oauth2:
    audiences:
      - myapp
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error when auth.oauth2.issuer is missing")
	}
}

func TestLoad_Auth_OAuth2_MissingAudiences_Error(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    inference_url: "http://inference.svc"
auth:
  mode: oauth2
  oauth2:
    issuer: https://idp.example.com
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error when auth.oauth2.audiences is empty")
	}
}

func TestLoad_Auth_OAuth2_Valid_Parses(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    inference_url: "http://inference.svc"
auth:
  mode: oauth2
  oauth2:
    issuer: https://idp.example.com
    jwks_url: https://idp.example.com/.well-known/jwks.json
    audiences:
      - myapp
      - otherapp
    claims:
      subject: sub
      consumer: preferred_username
      scopes: scope
      groups: groups
      roles: realm_access.roles
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.Mode != "oauth2" {
		t.Errorf("expected mode=oauth2, got %q", cfg.Auth.Mode)
	}
	if cfg.Auth.OAuth2.Issuer != "https://idp.example.com" {
		t.Errorf("expected issuer, got %q", cfg.Auth.OAuth2.Issuer)
	}
	if cfg.Auth.OAuth2.JWKSURL != "https://idp.example.com/.well-known/jwks.json" {
		t.Errorf("expected jwks_url, got %q", cfg.Auth.OAuth2.JWKSURL)
	}
	if len(cfg.Auth.OAuth2.Audiences) != 2 {
		t.Errorf("expected 2 audiences, got %d", len(cfg.Auth.OAuth2.Audiences))
	}
	if cfg.Auth.OAuth2.Claims.Consumer != "preferred_username" {
		t.Errorf("expected consumer claim=preferred_username, got %q", cfg.Auth.OAuth2.Claims.Consumer)
	}
	if cfg.Auth.OAuth2.Claims.Roles != "realm_access.roles" {
		t.Errorf("expected roles claim=realm_access.roles, got %q", cfg.Auth.OAuth2.Claims.Roles)
	}
}

func TestLoad_Auth_Proxy_Valid_Parses(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    inference_url: "http://inference.svc"
auth:
  mode: proxy
  proxy:
    consumer_header: X-Consumer-Username
    user_type_header: X-User-Type
    groups_header: X-Groups
    roles_header: X-Roles
    scopes_header: X-Scopes
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Auth.Mode != "proxy" {
		t.Errorf("expected mode=proxy, got %q", cfg.Auth.Mode)
	}
	if cfg.Auth.Proxy.ConsumerHeader != "X-Consumer-Username" {
		t.Errorf("expected consumer_header, got %q", cfg.Auth.Proxy.ConsumerHeader)
	}
	if cfg.Auth.Proxy.GroupsHeader != "X-Groups" {
		t.Errorf("expected groups_header, got %q", cfg.Auth.Proxy.GroupsHeader)
	}
}

func TestLoad_Auth_ProxyMode_NoHeaders_OK(t *testing.T) {
	// proxy mode with no headers configured is valid (headers may come from server.*).
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    inference_url: "http://inference.svc"
auth:
  mode: proxy
`)
	_, err := config.Load(path)
	if err != nil {
		t.Errorf("proxy mode with no headers should be valid, got: %v", err)
	}
}

// ── Policies config tests ─────────────────────────────────────────────────────

func TestLoad_Policies_Absent_IsNil(t *testing.T) {
	// No policies block → Policies field is nil.
	path := writeConfig(t, minimalValid)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Policies != nil {
		t.Errorf("expected Policies to be nil when absent, got %+v", cfg.Policies)
	}
}

func TestLoad_Policies_Parses(t *testing.T) {
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
    inference_url: "http://llm.svc"
auth:
  mode: proxy
  proxy:
    consumer_header: X-Consumer-Username
policies:
  default: deny
  rules:
    - match:
        groups:
          - research-lab
        roles:
          - analyst
        scopes:
          - llm:use
        consumers:
          - svc-account-1
        user_types:
          - premium
      allow_models:
        - "gpt-4o"
        - "chat-*"
      allow_service_types:
        - "llm"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Policies == nil {
		t.Fatal("expected Policies to be non-nil")
	}
	if cfg.Policies.Default != "deny" {
		t.Errorf("expected default=deny, got %q", cfg.Policies.Default)
	}
	if len(cfg.Policies.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(cfg.Policies.Rules))
	}
	r := cfg.Policies.Rules[0]
	if len(r.Match.Groups) != 1 || r.Match.Groups[0] != "research-lab" {
		t.Errorf("unexpected groups: %v", r.Match.Groups)
	}
	if len(r.Match.Roles) != 1 || r.Match.Roles[0] != "analyst" {
		t.Errorf("unexpected roles: %v", r.Match.Roles)
	}
	if len(r.Match.Scopes) != 1 || r.Match.Scopes[0] != "llm:use" {
		t.Errorf("unexpected scopes: %v", r.Match.Scopes)
	}
	if len(r.Match.Consumers) != 1 || r.Match.Consumers[0] != "svc-account-1" {
		t.Errorf("unexpected consumers: %v", r.Match.Consumers)
	}
	if len(r.Match.UserTypes) != 1 || r.Match.UserTypes[0] != "premium" {
		t.Errorf("unexpected user_types: %v", r.Match.UserTypes)
	}
	if len(r.AllowModels) != 2 {
		t.Errorf("expected 2 allow_models, got %d", len(r.AllowModels))
	}
	if len(r.AllowServiceTypes) != 1 || r.AllowServiceTypes[0] != "llm" {
		t.Errorf("unexpected allow_service_types: %v", r.AllowServiceTypes)
	}
}

func TestLoad_Policies_InvalidDefault_Error(t *testing.T) {
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
    inference_url: "http://llm.svc"
auth:
  mode: proxy
policies:
  default: permit
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for invalid policies.default")
	}
}

func TestLoad_Policies_WithRules_RequiresAuthMode_Error(t *testing.T) {
	// policies with rules but no auth.mode → error.
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
    inference_url: "http://llm.svc"
policies:
  default: deny
  rules:
    - match:
        groups:
          - admins
      allow_models:
        - "*"
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error when policies have rules but auth.mode is empty")
	}
}

func TestLoad_Policies_DefaultDenyNoRules_RequiresAuthMode_Error(t *testing.T) {
	// policies with default=deny (even no rules) but no auth.mode → error.
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
    inference_url: "http://llm.svc"
policies:
  default: deny
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error when policies.default=deny but auth.mode is empty")
	}
}

func TestLoad_Policies_WithAuthMode_OK(t *testing.T) {
	// policies + auth.mode set → valid.
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
    inference_url: "http://llm.svc"
auth:
  mode: proxy
policies:
  default: deny
  rules:
    - match:
        roles:
          - admin
      allow_models:
        - "*"
`)
	_, err := config.Load(path)
	if err != nil {
		t.Errorf("policies + auth.mode should be valid, got: %v", err)
	}
}

func TestLoad_Policies_DefaultAllow_NoAuthRequired(t *testing.T) {
	// Default "allow" disables enforcement; no auth.mode needed.
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
    inference_url: "http://llm.svc"
policies:
  default: allow
`)
	_, err := config.Load(path)
	if err != nil {
		t.Errorf("policies.default=allow without auth.mode should be valid, got: %v", err)
	}
}
