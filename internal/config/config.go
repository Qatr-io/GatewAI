package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server        ServerConfig                          `yaml:"server"`
	Kafka         KafkaConfig                           `yaml:"kafka"`
	S3            S3Config                              `yaml:"s3"`
	Redis         RedisConfig                           `yaml:"redis"`
	Services      []ServiceConfig                       `yaml:"services"`
	Encryption    EncryptionConfig                      `yaml:"encryption"`
	Metrics       MetricsConfig                         `yaml:"metrics"`
	SemanticCache SemanticCacheConfig                   `yaml:"semantic_cache"`
	// RateLimits maps service type → user type → limit.
	// User type "*" is the fallback applied when the user_type_header is absent
	// or the specific type has no entry.
	// Leave empty to disable rate limiting.
	RateLimits map[string]map[string]RateLimitConfig `yaml:"rate_limits"`
}

// SemanticCacheConfig configures the global semantic (vector-similarity) cache.
// Requires Redis Stack (RediSearch module) and an embedding service.
// Disabled when EmbeddingServiceURL is empty.
type SemanticCacheConfig struct {
	// EmbeddingServiceURL is the base URL of an OpenAI-compatible embeddings
	// service (e.g. "http://gateway.svc:8080" to self-call the gateway).
	EmbeddingServiceURL string `yaml:"embedding_service_url"`
	// EmbeddingModel is the model name passed to the embeddings endpoint.
	EmbeddingModel string `yaml:"embedding_model"`
	// Threshold is the minimum cosine similarity to consider a semantic hit (0–1).
	// Defaults to 0.95 when not set.
	Threshold float64 `yaml:"threshold"`
	// EmbeddingDim is the vector dimension returned by the embedding model.
	// Defaults to 1024 (bge-m3) when not set.
	EmbeddingDim int `yaml:"embedding_dim"`
}

// RateLimitConfig defines the allowed request rate for a (service, user-type) pair.
type RateLimitConfig struct {
	Rate   int    `yaml:"rate"`   // max requests per Period
	Period string `yaml:"period"` // e.g. "1m", "1h", "24h"
}

// MetricsConfig controls optional high-cardinality metric features.
type MetricsConfig struct {
	// TopConsumers sets how many top consumers to expose in Prometheus via a
	// Redis sorted-set backed GaugeVec, refreshed every 60s. 0 = disabled.
	TopConsumers int `yaml:"top_consumers"`
	// ConsumerLabels enables direct per-consumer Prometheus labels.
	// Only suitable for deployments with a small, bounded number of consumers
	// (< 50). Disabled by default — use TopConsumers for large deployments.
	ConsumerLabels bool `yaml:"consumer_labels"`
}

type EncryptionConfig struct {
	// Key is a hex-encoded 32-byte AES-256 key. Empty = encryption disabled.
	Key string `yaml:"key"`
}

type ServerConfig struct {
	Addr         string        `yaml:"addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
	// PriorityHeader is the HTTP header injected by APISIX on SA consumer
	// requests to signal high-priority processing. When a request carries this
	// header and the service has a priority_topic configured, the job is
	// published to that topic instead of input_topic. The relay processes
	// priority-topic jobs via POST /sync, which defers normal async jobs
	// (identical mechanism to sync-over-Kafka priority).
	PriorityHeader string `yaml:"priority_header"`
	// ConsumerHeader is the HTTP header used to identify the API consumer.
	// Typically set by APISIX after authentication (e.g. "X-Consumer-Username").
	// When configured:
	//   - the consumer name is stored in the job record and tracked in a Redis
	//     sorted set for listing via GET /jobs
	//   - kevent_jobs_by_consumer_total metric is incremented per consumer
	//   - GET /jobs/{service_type}/{id} enforces ownership: if the header is
	//     present, the job's consumer_name must match or 404 is returned
	// Leave empty in deployments without upstream authentication.
	ConsumerHeader string `yaml:"consumer_header"`
	// UserTypeHeader is the HTTP header carrying the consumer type (e.g. "sa" or
	// "user"), typically set by the APISIX Lua plugin after token introspection
	// (e.g. "X-User-Type"). Used for rate limiting (matched against
	// rate_limits[type][user_type]) and for LLM consumer metric labelling.
	// "*" is the fallback when the header is absent. Leave empty to disable
	// per-type differentiation.
	UserTypeHeader string `yaml:"user_type_header"`
}

type KafkaConfig struct {
	Brokers       []string   `yaml:"brokers"`
	ConsumerGroup string     `yaml:"consumer_group"`
	SASL          SASLConfig `yaml:"sasl"`
	TLS           TLSConfig  `yaml:"tls"`
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

// S3Config holds S3-compatible object storage credentials and settings.
type S3Config struct {
	Endpoint  string `yaml:"endpoint"`   // e.g. https://s3.fr-par.scw.cloud
	Region    string `yaml:"region"`     // e.g. fr-par
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	JobTTLH  int    `yaml:"job_ttl_hours"`
}

// BackendConfig describes one backend in a multi-backend list.
type BackendConfig struct {
	URL     string            `yaml:"url"`
	Weight  int               `yaml:"weight"`  // relative weight; 0 = fallback-only
	Headers map[string]string `yaml:"headers"` // headers added to every request to this backend; override service-level inference_headers
	Model   string            `yaml:"model"`   // real model name sent to this backend; overrides service-level backend_model
}

// ServiceConfig declares a single inference service type.
// New services are added here (config.yaml) — no Go code required.
type ServiceConfig struct {
	Type string `yaml:"type"`
	// Sync / OpenAI-compatible mode (optional).
	// Model is the value of the "model" field in the OpenAI payload used to
	// route the request to the correct InferenceService backend.
	Model string `yaml:"model"`
	// Default marks this model as the default for its service type.
	// Used when a request omits the model field and multiple models are configured.
	// At most one entry per type should be marked as default.
	Default bool `yaml:"default"`
	// Operations maps operation names to their URL paths.
	// e.g. {"transcription": ["/v1/audio/transcriptions"], "translation": ["/v1/audio/translations"]}
	// Multiple paths per operation are all indexed for sync routing; the first is used for async.
	Operations   map[string][]string `yaml:"operations"`
	InferenceURL string              `yaml:"inference_url"` // InferenceService cluster URL (single backend, legacy)
	// Backends is a list of inference backends for this service. When set, takes
	// precedence over inference_url and enables blue/green, canary, and fallback routing.
	// weight > 0 = eligible for primary selection (weighted random).
	// weight = 0 = fallback-only (tried only if all weight>0 backends fail).
	Backends []BackendConfig `yaml:"backends"`
	// Async / Kafka mode.
	InputTopic string `yaml:"input_topic"`
	ResultTopic string `yaml:"result_topic"`
	// SyncTopic is the dedicated Kafka topic for priority (sync-over-Kafka) jobs.
	// When set, POST /v1/* multipart requests are routed through Kafka instead of
	// proxied directly, giving them priority over async jobs via a second KafkaSource.
	SyncTopic string `yaml:"sync_topic"`
	// PriorityTopic is the Kafka topic for high-priority async jobs (e.g. SA accounts).
	// When set and the server.priority_header is present on the request, the job is
	// published here instead of input_topic. A dedicated KafkaSource routes this topic
	// to POST /sync on the relay, which defers normal async jobs for its duration.
	PriorityTopic string `yaml:"priority_topic"`
	AcceptedExts  []string `yaml:"accepted_exts"`
	MaxFileSizeMB int64    `yaml:"max_file_size_mb"`
	// SwaggerURL is an optional URL to an OpenAPI JSON spec for this service.
	// Fetched once at startup; served at GET /swagger/{type}/{model}.
	// Failures are logged as warnings and do not block startup.
	SwaggerURL string `yaml:"swagger_url"`
	// SwaggerHeaders are optional HTTP headers sent when fetching SwaggerURL.
	// Values support ${VAR} env expansion (same as the rest of config.yaml).
	// Example for a private GitHub release asset:
	//   swagger_headers:
	//     Accept: application/octet-stream
	//     Authorization: "Bearer ${GITHUB_TOKEN}"
	SwaggerHeaders map[string]string `yaml:"swagger_headers"`
	// InferenceHeaders are optional HTTP headers injected on every request
	// forwarded to the inference backend (sync-direct proxy only).
	// Values support ${VAR} env expansion (same as the rest of config.yaml).
	// These headers override anything sent by the client with the same name.
	// Example:
	//   inference_headers:
	//     Authorization: "Bearer ${RERANKER_API_KEY}"
	//     X-Api-Key: "${BACKEND_KEY}"
	InferenceHeaders map[string]string `yaml:"inference_headers"`
	// BackendModel is the real model name sent to the backend. Allows clients to use
	// a short alias (model field) while the gateway rewrites it to the backend's
	// expected identifier (e.g. "meta-llama/Meta-Llama-3-8B-Instruct" for vLLM).
	// Only applied when Provider is set. Empty means the alias is forwarded as-is.
	BackendModel string `yaml:"backend_model"`
	// Provider selects the LLM backend protocol. When set, JSON requests are routed
	// through the LLM proxy handler instead of the bare direct proxy.
	// Valid values: "openai", "anthropic", "ollama", "passthrough". Empty = legacy direct proxy.
	Provider string `yaml:"provider"`
	// ResponseCacheTTL is the TTL in seconds for caching LLM responses in Redis.
	// 0 = caching disabled. Only applied when Provider is set.
	ResponseCacheTTL int `yaml:"response_cache_ttl"`
	// Retries is the number of additional full backend-cycle attempts when all
	// backends return a network error or a 5xx response. 0 = no retry (default).
	// Only applies to the sync-direct proxy path (not Kafka-based flows).
	// A 500ms exponential backoff is applied between each cycle.
	Retries int `yaml:"retries"`
	// SemanticCacheTTL is the TTL in seconds for storing LLM responses in the
	// semantic (vector-similarity) cache. 0 = semantic caching disabled for this
	// service. Requires the global semantic_cache to be configured.
	SemanticCacheTTL int `yaml:"semantic_cache_ttl"`
}

// Load reads and validates the YAML config file at path.
// Values of the form ${VAR} or ${VAR:-default} are expanded from the environment.
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

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// expandWithDefault gère la syntaxe ${VAR:-default} que os.ExpandEnv ne supporte pas.
// Si la variable est définie et non vide, sa valeur est retournée ; sinon le défaut est utilisé.
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

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 120 * time.Second
	}
	// WriteTimeout has no default: 0 means no timeout, which is correct for a
	// gateway that handles long-running sync-over-Kafka jobs (OCR, large audio).
	// Set explicitly in config if a hard limit is desired.
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = 120 * time.Second
	}
	if c.Kafka.ConsumerGroup == "" {
		c.Kafka.ConsumerGroup = "kevent-gateway"
	}
	if c.Redis.JobTTLH == 0 {
		c.Redis.JobTTLH = 72
	}
	for i := range c.Services {
		if c.Services[i].MaxFileSizeMB == 0 {
			c.Services[i].MaxFileSizeMB = 100
		}
	}
}

func (c *Config) validate() error {
	if c.S3.Endpoint == "" {
		return fmt.Errorf("s3.endpoint is required")
	}
	if c.S3.Region == "" {
		return fmt.Errorf("s3.region is required")
	}
	if c.S3.Bucket == "" {
		return fmt.Errorf("s3.bucket is required")
	}
	if c.Redis.Addr == "" {
		return fmt.Errorf("redis.addr is required")
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("at least one service must be configured")
	}
	needsKafka := false
	for _, svc := range c.Services {
		if svc.Type == "" {
			return fmt.Errorf("a service has an empty type")
		}
		// input_topic and result_topic must both be set or both be empty.
		if (svc.InputTopic == "") != (svc.ResultTopic == "") {
			return fmt.Errorf("service %q: input_topic and result_topic must both be set or both be empty", svc.Type)
		}
		if svc.InputTopic != "" || svc.ResultTopic != "" || svc.SyncTopic != "" || svc.PriorityTopic != "" {
			needsKafka = true
		}
	}
	if needsKafka && len(c.Kafka.Brokers) == 0 {
		return fmt.Errorf("kafka.brokers is required when services use Kafka topics")
	}
	validProviders := map[string]bool{"openai": true, "anthropic": true, "ollama": true, "passthrough": true}
	for _, svc := range c.Services {
		if svc.Provider != "" && !validProviders[svc.Provider] {
			return fmt.Errorf("service %q: unknown provider %q (valid: openai, anthropic, ollama, passthrough)", svc.Type, svc.Provider)
		}
		if svc.ResponseCacheTTL < 0 {
			return fmt.Errorf("service %q: response_cache_ttl must be >= 0", svc.Type)
		}
		for i, b := range svc.Backends {
			if b.URL == "" {
				return fmt.Errorf("service %q: backends[%d].url must not be empty", svc.Type, i)
			}
			if b.Weight < 0 {
				return fmt.Errorf("service %q: backends[%d].weight must be >= 0", svc.Type, i)
			}
		}
	}
	return nil
}
