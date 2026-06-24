package telemetry

import "time"

// OtelConfig is the root OpenTelemetry configuration block.
// Set enabled: true and configure at least exporter.endpoint to activate.
type OtelConfig struct {
	Enabled     bool           `yaml:"enabled"`
	ServiceName string         `yaml:"service_name"` // overrides the default service name in traces/logs
	Exporter    ExporterConfig `yaml:"exporter"`
	Traces      TracesConfig   `yaml:"traces"`
	Metrics     MetricsConfig  `yaml:"metrics"`
	Logs        LogsConfig     `yaml:"logs"`
}

// ExporterConfig holds the base OTLP endpoint shared across all signals.
// Each signal can override endpoint and headers individually.
type ExporterConfig struct {
	Endpoint string            `yaml:"endpoint"` // e.g. "http://otel-collector:4318"
	Headers  map[string]string `yaml:"headers"`  // e.g. {"Authorization": "Bearer ${TOKEN}"}
	Insecure bool              `yaml:"insecure"` // skip TLS verification
	Protocol string            `yaml:"protocol"` // "http" (default) | "grpc"
}

// TracesConfig configures distributed tracing.
type TracesConfig struct {
	Enabled     bool              `yaml:"enabled"`
	Endpoint    string            `yaml:"endpoint"`     // overrides exporter.endpoint
	Headers     map[string]string `yaml:"headers"`      // merged with exporter.headers
	SampleRatio float64           `yaml:"sample_ratio"` // 0.0–1.0; default 1.0
}

// MetricsConfig configures OTLP metrics push.
// Prometheus scraping remains available independently of this setting.
type MetricsConfig struct {
	Enabled  bool              `yaml:"enabled"`
	Endpoint string            `yaml:"endpoint"`
	Headers  map[string]string `yaml:"headers"`
	Interval string            `yaml:"interval"` // duration string e.g. "60s"; default 60s
}

// IntervalDuration parses Interval. Returns 60s if unset or invalid.
func (m MetricsConfig) IntervalDuration() time.Duration {
	if d, err := time.ParseDuration(m.Interval); err == nil && d > 0 {
		return d
	}
	return 60 * time.Second
}

// LogsConfig configures OTLP log export via the slog bridge.
type LogsConfig struct {
	Enabled  bool              `yaml:"enabled"`
	Endpoint string            `yaml:"endpoint"`
	Headers  map[string]string `yaml:"headers"`
}

// resolveSignal merges base exporter settings with per-signal overrides.
// Signal-level endpoint and headers take precedence over base values.
func resolveSignal(base ExporterConfig, signalEndpoint string, signalHeaders map[string]string) (endpoint string, headers map[string]string) {
	endpoint = base.Endpoint
	if signalEndpoint != "" {
		endpoint = signalEndpoint
	}
	headers = make(map[string]string, len(base.Headers)+len(signalHeaders))
	for k, v := range base.Headers {
		headers[k] = v
	}
	for k, v := range signalHeaders {
		headers[k] = v
	}
	return endpoint, headers
}
