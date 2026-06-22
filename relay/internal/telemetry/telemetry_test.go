package telemetry_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"gatewai/relay/internal/telemetry"
)

// ── Setup: disabled mode ──────────────────────────────────────────────────────

func TestSetup_Disabled(t *testing.T) {
	cfg := telemetry.OtelConfig{Enabled: false}
	tel, shutdown, err := telemetry.Setup(context.Background(), cfg, "relay-test", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())
	if tel == nil {
		t.Fatal("expected non-nil Telemetry")
	}
	if tel.Tracer == nil {
		t.Fatal("expected non-nil Tracer (no-op)")
	}
	if tel.Meter == nil {
		t.Fatal("expected non-nil Meter (no-op)")
	}
}

func TestSetup_Disabled_LogsMessage(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	cfg := telemetry.OtelConfig{Enabled: false}
	_, shutdown, _ := telemetry.Setup(context.Background(), cfg, "relay-svc", "v1")
	defer shutdown(context.Background())

	if !strings.Contains(buf.String(), "opentelemetry disabled") {
		t.Errorf("expected 'opentelemetry disabled' in logs, got: %s", buf.String())
	}
}

// ── Setup: enabled — construction ────────────────────────────────────────────

// OTLP exporters connect lazily; Setup must succeed even when unreachable.

func TestSetup_Enabled_TracesOnly(t *testing.T) {
	cfg := telemetry.OtelConfig{
		Enabled: true,
		Exporter: telemetry.ExporterConfig{
			Endpoint: "http://localhost:0",
			Insecure: true,
		},
		Traces: telemetry.TracesConfig{
			Enabled:     true,
			SampleRatio: 1.0,
		},
	}
	tel, shutdown, err := telemetry.Setup(context.Background(), cfg, "relay-traces", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())
	if tel.Tracer == nil {
		t.Fatal("expected non-nil Tracer")
	}
}

func TestSetup_Enabled_AllSignals(t *testing.T) {
	cfg := telemetry.OtelConfig{
		Enabled: true,
		Exporter: telemetry.ExporterConfig{
			Endpoint: "http://localhost:0",
			Insecure: true,
		},
		Traces:  telemetry.TracesConfig{Enabled: true, SampleRatio: 1.0},
		Metrics: telemetry.MetricsConfig{Enabled: true, Interval: "10s"},
		Logs:    telemetry.LogsConfig{Enabled: true},
	}
	tel, shutdown, err := telemetry.Setup(context.Background(), cfg, "relay-all", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())
	if tel.Tracer == nil || tel.Meter == nil {
		t.Fatal("expected non-nil Tracer and Meter")
	}
}

// ── Setup: startup log ────────────────────────────────────────────────────────

func TestSetup_Enabled_LogsEndpoint(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	cfg := telemetry.OtelConfig{
		Enabled: true,
		Exporter: telemetry.ExporterConfig{
			Endpoint: "http://relay-collector.example:4318",
			Insecure: true,
		},
		Traces: telemetry.TracesConfig{Enabled: true},
	}
	_, shutdown, err := telemetry.Setup(context.Background(), cfg, "relay-svc", "v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())

	logged := buf.String()
	if !strings.Contains(logged, "opentelemetry initialising") {
		t.Errorf("expected startup log, got: %s", logged)
	}
	if !strings.Contains(logged, "relay-collector.example:4318") {
		t.Errorf("expected endpoint in startup log, got: %s", logged)
	}
}

// ── Setup: W3C propagator ─────────────────────────────────────────────────────

func TestSetup_Enabled_RegistersPropagator(t *testing.T) {
	cfg := telemetry.OtelConfig{
		Enabled:  true,
		Exporter: telemetry.ExporterConfig{Endpoint: "http://localhost:0", Insecure: true},
		Traces:   telemetry.TracesConfig{Enabled: true},
	}
	_, shutdown, err := telemetry.Setup(context.Background(), cfg, "relay-prop", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())

	fields := otel.GetTextMapPropagator().Fields()
	found := false
	for _, f := range fields {
		if f == "traceparent" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'traceparent' in propagator fields, got: %v", fields)
	}
}

func TestSetup_Disabled_DoesNotOverridePropagator(t *testing.T) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.Baggage{}))
	t.Cleanup(func() { otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator()) })

	cfg := telemetry.OtelConfig{Enabled: false}
	_, shutdown, err := telemetry.Setup(context.Background(), cfg, "relay-test", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())

	for _, f := range otel.GetTextMapPropagator().Fields() {
		if f == "traceparent" {
			t.Error("disabled Setup must not add 'traceparent' to the global propagator")
		}
	}
}

// ── Setup: OTel SDK errors via slog ──────────────────────────────────────────

func TestSetup_Enabled_OtelErrorsAppearInSlog(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	cfg := telemetry.OtelConfig{
		Enabled:  true,
		Exporter: telemetry.ExporterConfig{Endpoint: "http://localhost:0", Insecure: true},
		Traces:   telemetry.TracesConfig{Enabled: true},
	}
	_, shutdown, err := telemetry.Setup(context.Background(), cfg, "relay-errors", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())

	otel.Handle(errSentinel("relay synthetic otel error"))

	if !strings.Contains(buf.String(), "relay synthetic otel error") {
		t.Errorf("expected synthetic error in slog output, got: %s", buf.String())
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// ── Setup: shutdown ───────────────────────────────────────────────────────────

func TestSetup_Enabled_ShutdownFlushes(t *testing.T) {
	cfg := telemetry.OtelConfig{
		Enabled:  true,
		Exporter: telemetry.ExporterConfig{Endpoint: "http://localhost:0", Insecure: true},
		Traces:   telemetry.TracesConfig{Enabled: true},
	}
	_, shutdown, err := telemetry.Setup(context.Background(), cfg, "relay-flush", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Logf("shutdown returned (expected on unreachable endpoint): %v", err)
	}
}

// ── MetricsConfig.IntervalDuration ───────────────────────────────────────────

func TestMetricsConfig_IntervalDuration(t *testing.T) {
	tests := []struct {
		interval string
		wantSecs float64
	}{
		{"30s", 30},
		{"2m", 120},
		{"1h", 3600},
		{"",      60},
		{"bad",   60},
		{"-1s",   60},
	}
	for _, tc := range tests {
		cfg := telemetry.MetricsConfig{Interval: tc.interval}
		got := cfg.IntervalDuration().Seconds()
		if got != tc.wantSecs {
			t.Errorf("Interval=%q: got %vs, want %vs", tc.interval, got, tc.wantSecs)
		}
	}
}
