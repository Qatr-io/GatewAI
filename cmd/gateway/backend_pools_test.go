package main

import (
	"reflect"
	"sort"
	"testing"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/llmproxy"
	"gatewai/gateway/internal/service"
)

func TestBuildPoolLimits(t *testing.T) {
	pools := map[string]config.BackendPoolConfig{
		"with-rate": {RateLimit: config.RateLimitConfig{Rate: 50, Period: "1s"}},
		"no-rate":   {MaxConcurrent: 10}, // no RateLimit.Rate set
	}
	got := buildPoolLimits(pools)
	want := map[string]config.RateLimitConfig{
		"with-rate": {Rate: 50, Period: "1s"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildPoolLimits() = %+v, want %+v", got, want)
	}
}

func TestBuildPoolLimits_Empty(t *testing.T) {
	if got := buildPoolLimits(nil); len(got) != 0 {
		t.Fatalf("buildPoolLimits(nil) = %+v, want empty", got)
	}
	if got := buildPoolLimits(map[string]config.BackendPoolConfig{}); len(got) != 0 {
		t.Fatalf("buildPoolLimits({}) = %+v, want empty", got)
	}
}

func TestBuildPoolMemberLimits(t *testing.T) {
	pools := map[string]config.BackendPoolConfig{
		"vllm-llama3": {
			Members: []config.BackendPoolMember{
				{
					BackendConfig: config.BackendConfig{URL: "http://vllm-1:8000"},
					RateLimit:     config.RateLimitConfig{Rate: 20, Period: "1s"},
				},
				{
					BackendConfig: config.BackendConfig{URL: "http://vllm-2:8000"},
					// no member-level rate limit
				},
			},
		},
	}
	got := buildPoolMemberLimits(pools)
	want := map[string]map[string]config.RateLimitConfig{
		"vllm-llama3": {
			"http://vllm-1:8000": {Rate: 20, Period: "1s"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildPoolMemberLimits() = %+v, want %+v", got, want)
	}
}

func TestBuildPoolMemberLimits_Empty(t *testing.T) {
	if got := buildPoolMemberLimits(nil); len(got) != 0 {
		t.Fatalf("buildPoolMemberLimits(nil) = %+v, want empty", got)
	}
	pools := map[string]config.BackendPoolConfig{
		"vllm-llama3": {Members: []config.BackendPoolMember{
			{BackendConfig: config.BackendConfig{URL: "http://vllm-1:8000"}}, // no rate limit
		}},
	}
	if got := buildPoolMemberLimits(pools); len(got) != 0 {
		t.Fatalf("buildPoolMemberLimits() = %+v, want empty (no member configures a rate)", got)
	}
}

func sortedProbeTargets(targets []llmproxy.ProbeTarget) []llmproxy.ProbeTarget {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].URL != targets[j].URL {
			return targets[i].URL < targets[j].URL
		}
		return targets[i].Model < targets[j].Model
	})
	return targets
}

func TestDedupeProbeTargets_SameURLAcrossTwoAliases_ProbedOnce(t *testing.T) {
	// Two services sharing one backend_pools entry — the exact scenario that
	// motivated the dedup: without it, the shared URL would be probed twice
	// per interval (once per alias) instead of once per physical backend.
	pool := map[string]config.BackendPoolConfig{
		"vllm-llama3": {Members: []config.BackendPoolMember{
			{BackendConfig: config.BackendConfig{URL: "http://vllm-1:8000", Weight: 1}},
		}},
	}
	cfgs := []config.ServiceConfig{
		{
			Type:        "llm",
			Model:       "gpt-4o",
			BackendPool: "vllm-llama3",
			Operations:  map[string][]string{"chat": {"/v1/chat/completions"}},
		},
		{
			Type:        "llm",
			Model:       "llama3-chat",
			BackendPool: "vllm-llama3",
			Operations:  map[string][]string{"chat": {"/v1/chat/completions"}},
		},
	}
	reg := service.NewRegistry(cfgs, service.WithBackendPools(pool))

	targets := dedupeProbeTargets(reg.Models())
	if len(targets) != 1 {
		t.Fatalf("expected exactly 1 deduplicated probe target for the shared URL, got %d: %+v", len(targets), targets)
	}
	if targets[0].URL != "http://vllm-1:8000" {
		t.Fatalf("unexpected probe target URL: %+v", targets[0])
	}
}

func TestDedupeProbeTargets_DifferentHealthPath_NotDeduped(t *testing.T) {
	// Same URL but a different per-service HealthCheck.Path is a distinct probe.
	cfgs := []config.ServiceConfig{
		{
			Type:         "llm",
			Model:        "model-a",
			InferenceURL: "http://shared:8000",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			Health:       config.ServiceHealthConfig{Path: "/health-a"},
		},
		{
			Type:         "llm",
			Model:        "model-b",
			InferenceURL: "http://shared:8000",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			Health:       config.ServiceHealthConfig{Path: "/health-b"},
		},
	}
	reg := service.NewRegistry(cfgs)

	targets := sortedProbeTargets(dedupeProbeTargets(reg.Models()))
	if len(targets) != 2 {
		t.Fatalf("expected 2 distinct probe targets (different health paths), got %d: %+v", len(targets), targets)
	}
}

func TestDedupeProbeTargets_HealthCheckDisabled_Skipped(t *testing.T) {
	cfgs := []config.ServiceConfig{{
		Type:         "llm",
		Model:        "model-a",
		InferenceURL: "http://shared:8000",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		Health:       config.ServiceHealthConfig{Disabled: true},
	}}
	reg := service.NewRegistry(cfgs)

	targets := dedupeProbeTargets(reg.Models())
	if len(targets) != 0 {
		t.Fatalf("expected no probe targets when health check is disabled, got %+v", targets)
	}
}
