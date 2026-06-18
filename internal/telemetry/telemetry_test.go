package telemetry_test

import (
	"context"
	"testing"

	"gatewai/gateway/internal/telemetry"
)

func TestSetup_Disabled(t *testing.T) {
	cfg := telemetry.OtelConfig{Enabled: false}
	tel, shutdown, err := telemetry.Setup(context.Background(), cfg, "test", "dev")
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

func TestSetup_Enabled_TracesOnly(t *testing.T) {
	// Enabled with an unreachable endpoint — Setup should not fail at construction
	// (OTLP exporters connect lazily on first export, not at creation).
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
	tel, shutdown, err := telemetry.Setup(context.Background(), cfg, "test", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())
	if tel == nil {
		t.Fatal("expected non-nil Telemetry")
	}
	if tel.Tracer == nil {
		t.Fatal("expected non-nil Tracer")
	}
}

func TestSetup_ShutdownIdempotent(t *testing.T) {
	cfg := telemetry.OtelConfig{Enabled: false}
	_, shutdown, err := telemetry.Setup(context.Background(), cfg, "test", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	// Second call is a no-op for disabled mode.
	_ = shutdown(ctx)
}

func TestMetricsConfig_IntervalDuration(t *testing.T) {
	tests := []struct {
		interval string
		wantSecs float64
	}{
		{"30s", 30},
		{"2m", 120},
		{"", 60},      // default
		{"invalid", 60}, // fallback
	}
	for _, tc := range tests {
		cfg := telemetry.MetricsConfig{Interval: tc.interval}
		got := cfg.IntervalDuration().Seconds()
		if got != tc.wantSecs {
			t.Errorf("Interval=%q: got %vs, want %vs", tc.interval, got, tc.wantSecs)
		}
	}
}
