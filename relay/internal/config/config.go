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
//
// operation_timeouts is an optional per-operation override: the key is the
// inference path (e.g. "/v1/audio/diarizations") and the value is a
// time.ParseDuration-compatible string ("3600s", "1h", "45m"). Falls back to
// Timeout (or 300s default) for any path not listed. Useful because some
// inference operations (diarization on long audio, large batch embeddings…)
// can run an order of magnitude longer than others.
type InferenceConfig struct {
	BaseURL           string            `yaml:"base_url"`
	APIKey            string            `yaml:"api_key"`
	Timeout           string            `yaml:"timeout"`
	OperationTimeouts map[string]string `yaml:"operation_timeouts"`
	ExtraFields       map[string]string `yaml:"extra_fields"`
}

// TimeoutDuration returns the default timeout for this relay (used when no
// per-operation override applies). Falls back to 300s if not configured.
func (c InferenceConfig) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
		return d
	}
	return 300 * time.Second
}

// TimeoutFor returns the timeout for the given inference path, honoring
// the operation_timeouts override if set and parseable, otherwise the
// relay-wide default.
func (c InferenceConfig) TimeoutFor(inferencePath string) time.Duration {
	if raw, ok := c.OperationTimeouts[inferencePath]; ok {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return c.TimeoutDuration()
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
	Brokers []string   `yaml:"brokers"`
	SASL    SASLConfig `yaml:"sasl"`
	TLS     TLSConfig  `yaml:"tls"`
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
