package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Service    ServiceConfig    `yaml:"service"`
	Kafka      KafkaConfig      `yaml:"kafka"`
	S3         S3Config         `yaml:"s3"`
	Encryption EncryptionConfig `yaml:"encryption"`
	Inference  InferenceConfig  `yaml:"inference"`
}

// InferenceConfig holds the local inference endpoint configuration.
// The base_url is combined with the OpenAI path supplied per-event
// (InputEvent.InferenceURL) to build the actual request URL:
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

type EncryptionConfig struct {
	Key string `yaml:"key"`
}

type ServiceConfig struct {
	ResultTopic string `yaml:"result_topic"`
}

// Type derives the service type from the result topic.
// The expected format is "jobs.<type>.results" (e.g. "jobs.whisper.results" → "whisper").
// Falls back to the full topic name if the format doesn't match.
func (s ServiceConfig) Type() string {
	parts := strings.SplitN(s.ResultTopic, ".", 3)
	if len(parts) == 3 && parts[0] == "jobs" {
		return parts[1]
	}
	return s.ResultTopic
}

type KafkaConfig struct {
	Brokers       []string   `yaml:"brokers"`
	SASL          SASLConfig `yaml:"sasl"`
	TLS           TLSConfig  `yaml:"tls"`
	InputTopic    string     `yaml:"input_topic"`
	ConsumerGroup string     `yaml:"consumer_group"`
	// DLQTopic is the topic where failed jobs are published when an infra error
	// occurs after the offset has already been committed. Empty = no DLQ (infra
	// errors cause os.Exit(1) without re-queuing the job).
	DLQTopic string `yaml:"dlq_topic"`
}

type SASLConfig struct {
	Mechanism string `yaml:"mechanism"` // PLAIN | SCRAM-SHA-256 | SCRAM-SHA-512
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
}

type TLSConfig struct {
	Enabled    bool   `yaml:"enabled"`
	CACertPath string `yaml:"ca_cert_path"`
}

type S3Config struct {
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
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
	if c.Service.ResultTopic == "" {
		return fmt.Errorf("service.result_topic is required (set RESULT_TOPIC env var)")
	}
	if len(c.Kafka.Brokers) == 0 {
		return fmt.Errorf("kafka.brokers is required")
	}
	if c.Kafka.InputTopic == "" {
		return fmt.Errorf("kafka.input_topic is required (set KAFKA_INPUT_TOPIC env var)")
	}
	if c.Kafka.ConsumerGroup == "" {
		return fmt.Errorf("kafka.consumer_group is required (set KAFKA_CONSUMER_GROUP env var)")
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
	return nil
}
