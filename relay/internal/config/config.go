package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"gatewai/relay/internal/telemetry"
)

// Config holds all relay runtime configuration.
type Config struct {
	Model          string           `yaml:"model"`
	Redis          RedisConfig      `yaml:"redis"`
	S3             S3Config         `yaml:"s3"`
	Encryption     EncryptionConfig `yaml:"encryption"`
	Inference      InferenceConfig  `yaml:"inference"`
	// Gateway holds the connection details for the gateway's completion
	// callback endpoint (POST /-/relay/jobs/{id}/complete), called once after
	// the relay persists a job's result to Redis.
	Gateway GatewayConfig `yaml:"gateway"`
	// QueuePopTimeout is how long Pop waits for a job before returning ErrNoJob.
	// Use when a pod may start after its queue item was already cancelled.
	// Defaults to 30s. Set to "0" to block indefinitely (legacy behaviour).
	QueuePopTimeout string               `yaml:"queue_pop_timeout"`
	Otel            telemetry.OtelConfig `yaml:"opentelemetry"`
	// LogLevel sets the minimum log level: DEBUG, INFO, WARN, ERROR.
	// Defaults to INFO when absent or invalid.
	LogLevel string `yaml:"log_level"`
}

// QueuePopTimeoutDuration returns the configured queue pop timeout.
// "0" or "0s" means block indefinitely. Defaults to 30s.
func (c *Config) QueuePopTimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.QueuePopTimeout); err == nil {
		if d <= 0 {
			return 0
		}
		return d
	}
	return 30 * time.Second
}

// InferenceConfig holds the local inference endpoint configuration.
// The base_url is combined with the OpenAI path supplied per-job
// (Job.InferenceURL) to build the actual request URL:
//
//	base_url + inference_url  (e.g. "http://127.0.0.1:9000" + "/v1/audio/transcriptions")
//
// extra_fields contains optional form fields sent with every multipart request
// (e.g. response_format, language, prompt). Empty values are skipped.
type InferenceConfig struct {
	BaseURL            string            `yaml:"base_url"`
	APIKey             string            `yaml:"api_key"`
	Timeout            string            `yaml:"timeout"`
	ReadyTimeout       string            `yaml:"ready_timeout"`
	ReadyInterval      string            `yaml:"ready_interval"`
	HealthCheckTimeout string            `yaml:"health_check_timeout"`
	ExtraFields        map[string]string `yaml:"extra_fields"`
}

// TimeoutDuration returns the configured inference timeout.
// "0" or "0s" means no timeout (http.Client.Timeout = 0).
// Invalid or absent values fall back to 300s.
func (c InferenceConfig) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.Timeout); err == nil {
		if d <= 0 {
			return 0
		}
		return d
	}
	return 300 * time.Second
}

// ReadyTimeoutDuration returns how long to wait for the inference service to
// become healthy at startup. Invalid or absent values fall back to 10 minutes.
func (c InferenceConfig) ReadyTimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.ReadyTimeout); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

// ReadyIntervalDuration returns the polling interval between health checks
// during startup. Invalid or absent values fall back to 5 seconds.
func (c InferenceConfig) ReadyIntervalDuration() time.Duration {
	if d, err := time.ParseDuration(c.ReadyInterval); err == nil && d > 0 {
		return d
	}
	return 5 * time.Second
}

// HealthCheckTimeoutDuration returns the per-request timeout for health check
// HTTP calls. Invalid or absent values fall back to 2 seconds.
func (c InferenceConfig) HealthCheckTimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.HealthCheckTimeout); err == nil && d > 0 {
		return d
	}
	return 2 * time.Second
}

// GatewayConfig holds the gateway's in-cluster address for the completion callback.
type GatewayConfig struct {
	// BaseURL is the gateway's base URL, e.g.
	// "http://gatewai-gateway.default.svc.cluster.local". No default —
	// always supplied explicitly per-deployment (the Service name is derived
	// from the Helm release name), same convention as redis.addr.
	BaseURL string `yaml:"base_url"`
}

// EncryptionConfig holds the AES key for S3 payload encryption.
type EncryptionConfig struct {
	Key string `yaml:"key"`
}

// RedisConfig holds connection parameters for the relay's Redis instance.
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

// S3Config holds MinIO/S3-compatible storage credentials.
type S3Config struct {
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
	// CABundle is an optional path to a PEM file containing one or more CA
	// certificates used to verify the S3 endpoint's TLS certificate.
	// Required when the endpoint uses a private or self-signed CA.
	// Empty (default) uses the system certificate pool.
	CABundle string `yaml:"ca_bundle"`
	// UsePathStyle forces path-style URLs (https://endpoint/bucket/key) instead
	// of virtual-hosted style (https://bucket.endpoint/key).
	// Required for most S3-compatible providers. Default: true.
	UsePathStyle bool `yaml:"use_path_style"`
	// SSLInsecure disables TLS certificate verification for the S3 endpoint.
	// Use only in development or when the endpoint uses a self-signed cert
	// and ca_bundle is not available.
	SSLInsecure bool `yaml:"ssl_insecure"`
}

// SlogLevel returns the configured slog.Level, defaulting to INFO.
func (c *Config) SlogLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(c.LogLevel))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// Load reads, env-expands, and validates the YAML config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	expanded := []byte(os.Expand(string(data), expandWithDefault))

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// expandWithDefault handles the ${VAR:-default} syntax that os.ExpandEnv does not support.
func expandWithDefault(key string) string {
	if i := strings.Index(key, ":-"); i >= 0 {
		varName, defaultVal := key[:i], key[i+2:]
		if v, ok := os.LookupEnv(varName); ok && v != "" {
			return v
		}
		return defaultVal
	}
	return os.Getenv(key)
}

func (c *Config) validate() error {
	if c.Redis.Addr == "" {
		return fmt.Errorf("redis.addr is required")
	}
	if c.Model == "" {
		return fmt.Errorf("model is required")
	}
	if c.S3.Endpoint == "" {
		return fmt.Errorf("s3.endpoint is required")
	}
	if c.S3.Region == "" {
		return fmt.Errorf("s3.region is required")
	}
	if c.S3.Bucket == "" {
		return fmt.Errorf("s3.bucket is required")
	}
	if c.Inference.BaseURL == "" {
		return fmt.Errorf("inference.base_url is required")
	}
	if c.Gateway.BaseURL == "" {
		return fmt.Errorf("gateway.base_url is required")
	}
	return nil
}
