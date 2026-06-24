package authz_test

import (
	"testing"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/authz"
	"gatewai/gateway/internal/config"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func principal(groups, roles, scopes []string, consumer, userType string) *auth.Principal {
	return &auth.Principal{
		Subject:       consumer,
		Consumer:      consumer,
		Groups:        groups,
		Roles:         roles,
		Scopes:        scopes,
		UserType:      userType,
		Authenticated: true,
	}
}

// ── table-driven tests ────────────────────────────────────────────────────────

func TestEngine_Allowed(t *testing.T) {
	// Shared engine used by most cases.
	engine := authz.New(config.PoliciesConfig{
		Default: "deny",
		Rules: []config.PolicyRule{
			// Rule 0: research-lab group → gpt-oss-120b and chat-* models, any service.
			{
				Match:       config.PolicyMatch{Groups: []string{"research-lab"}},
				AllowModels: []string{"gpt-oss-120b", "chat-*"},
			},
			// Rule 1: admin role → all models (*), any service.
			{
				Match:       config.PolicyMatch{Roles: []string{"admin"}},
				AllowModels: []string{"*"},
			},
			// Rule 2: scope "llm:use" → chat-* model, only service type "llm".
			{
				Match:             config.PolicyMatch{Scopes: []string{"llm:use"}},
				AllowModels:       []string{"chat-*"},
				AllowServiceTypes: []string{"llm"},
			},
			// Rule 3: empty Match → matches everyone, allows "public-model".
			{
				Match:       config.PolicyMatch{},
				AllowModels: []string{"public-model"},
			},
			// Rule 4: AllowModels empty → grants nothing.
			{
				Match:       config.PolicyMatch{Groups: []string{"everybody"}},
				AllowModels: []string{},
			},
		},
	})

	tests := []struct {
		name        string
		principal   *auth.Principal
		serviceType string
		model       string
		want        bool
	}{
		// ── Group-scoped rule ────────────────────────────────────────────────
		{
			name:        "group research-lab allows explicit model gpt-oss-120b",
			principal:   principal([]string{"research-lab"}, nil, nil, "alice", "user"),
			serviceType: "llm",
			model:       "gpt-oss-120b",
			want:        true,
		},
		{
			name:        "group research-lab allows glob chat-small",
			principal:   principal([]string{"research-lab"}, nil, nil, "alice", "user"),
			serviceType: "llm",
			model:       "chat-small",
			want:        true,
		},
		{
			name:        "group research-lab allows glob chat-pro",
			principal:   principal([]string{"research-lab"}, nil, nil, "alice", "user"),
			serviceType: "audio",
			model:       "chat-pro",
			want:        true,
		},
		{
			name:        "group research-lab denies unlisted model secret-model",
			principal:   principal([]string{"research-lab"}, nil, nil, "alice", "user"),
			serviceType: "llm",
			model:       "secret-model",
			want:        false,
		},
		// ── Role-based wildcard rule ─────────────────────────────────────────
		{
			name:        "admin role allows any model",
			principal:   principal(nil, []string{"admin"}, nil, "bob", "sa"),
			serviceType: "llm",
			model:       "whatever-model",
			want:        true,
		},
		{
			name:        "admin role allows secret-model too",
			principal:   principal(nil, []string{"admin"}, nil, "bob", "sa"),
			serviceType: "audio",
			model:       "secret-model",
			want:        true,
		},
		// ── Scope rule with service-type constraint ──────────────────────────
		{
			name:        "scope llm:use allows chat-small for service llm",
			principal:   principal(nil, nil, []string{"llm:use"}, "carol", "user"),
			serviceType: "llm",
			model:       "chat-small",
			want:        true,
		},
		{
			name:        "scope llm:use denies chat-small for service audio",
			principal:   principal(nil, nil, []string{"llm:use"}, "carol", "user"),
			serviceType: "audio",
			model:       "chat-small",
			want:        false,
		},
		{
			name:        "scope llm:use denies gpt-oss-120b (not in rule) for service llm",
			principal:   principal(nil, nil, []string{"llm:use"}, "carol", "user"),
			serviceType: "llm",
			model:       "gpt-oss-120b",
			want:        false,
		},
		// ── No matching rule → deny ──────────────────────────────────────────
		{
			name:        "no matching rule → deny",
			principal:   principal([]string{"unknown-group"}, nil, nil, "dave", "user"),
			serviceType: "llm",
			model:       "some-model",
			want:        false,
		},
		// ── Empty Match matches everyone (rule 3) ────────────────────────────
		{
			name:        "empty match rule grants public-model to random user",
			principal:   principal(nil, nil, nil, "eve", "anonymous"),
			serviceType: "llm",
			model:       "public-model",
			want:        true,
		},
		{
			name:        "empty match rule does not grant other models",
			principal:   principal(nil, nil, nil, "eve", "anonymous"),
			serviceType: "llm",
			model:       "not-public",
			want:        false,
		},
		// ── Nil principal ────────────────────────────────────────────────────
		{
			name:        "nil principal is granted by empty-match rule (public-model)",
			principal:   nil,
			serviceType: "llm",
			model:       "public-model",
			want:        true,
		},
		{
			name:        "nil principal is denied when no empty-match rule covers the model",
			principal:   nil,
			serviceType: "llm",
			model:       "gpt-oss-120b",
			want:        false,
		},
		// ── AllowModels empty → grants nothing ───────────────────────────────
		{
			name:        "AllowModels empty grants nothing even for matching group",
			principal:   principal([]string{"everybody"}, nil, nil, "frank", "user"),
			serviceType: "llm",
			model:       "any-model",
			want:        false,
		},
		// ── Glob edge cases ──────────────────────────────────────────────────
		{
			name:        "glob chat-* matches chat-pro",
			principal:   principal([]string{"research-lab"}, nil, nil, "grace", "user"),
			serviceType: "llm",
			model:       "chat-pro",
			want:        true,
		},
		{
			name:        "glob chat-* does not match chatpro (no hyphen separator)",
			principal:   principal([]string{"research-lab"}, nil, nil, "grace", "user"),
			serviceType: "llm",
			model:       "chatpro",
			want:        false,
		},
		{
			name:        "glob chat-* does not match chat (no suffix)",
			principal:   principal([]string{"research-lab"}, nil, nil, "grace", "user"),
			serviceType: "llm",
			model:       "chat",
			want:        false,
		},
		{
			name:        "glob chat-* matches chat- (empty suffix after hyphen)",
			principal:   principal([]string{"research-lab"}, nil, nil, "grace", "user"),
			serviceType: "llm",
			model:       "chat-",
			want:        true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.Allowed(tc.principal, tc.serviceType, tc.model)
			if got != tc.want {
				t.Errorf("Allowed(%v, %q, %q) = %v, want %v",
					tc.principal, tc.serviceType, tc.model, got, tc.want)
			}
		})
	}
}

// TestEngine_DefaultAllow verifies that Default="allow" always returns true
// regardless of rules or principal.
func TestEngine_DefaultAllow(t *testing.T) {
	engine := authz.New(config.PoliciesConfig{
		Default: "allow",
		// This deny-all rule must be ignored.
		Rules: []config.PolicyRule{
			{
				Match:       config.PolicyMatch{Roles: []string{"nonexistent-role"}},
				AllowModels: []string{"secret"},
			},
		},
	})

	cases := []struct {
		p           *auth.Principal
		serviceType string
		model       string
	}{
		{nil, "llm", "any-model"},
		{principal(nil, nil, nil, "", ""), "audio", "another-model"},
		{principal([]string{"g"}, []string{"r"}, nil, "c", "t"), "vision", "restricted"},
	}
	for _, tc := range cases {
		if !engine.Allowed(tc.p, tc.serviceType, tc.model) {
			t.Errorf("Default=allow: Allowed should always return true, got false for (%v, %q, %q)",
				tc.p, tc.serviceType, tc.model)
		}
	}
}

// TestEngine_ConsumerMatch verifies consumer-based matching.
func TestEngine_ConsumerMatch(t *testing.T) {
	engine := authz.New(config.PoliciesConfig{
		Default: "deny",
		Rules: []config.PolicyRule{
			{
				Match:       config.PolicyMatch{Consumers: []string{"svc-account-1", "svc-account-2"}},
				AllowModels: []string{"internal-*"},
			},
		},
	})

	t.Run("listed consumer allowed", func(t *testing.T) {
		p := principal(nil, nil, nil, "svc-account-1", "sa")
		if !engine.Allowed(p, "llm", "internal-fast") {
			t.Error("expected allowed for svc-account-1")
		}
	})
	t.Run("unlisted consumer denied", func(t *testing.T) {
		p := principal(nil, nil, nil, "svc-account-99", "sa")
		if engine.Allowed(p, "llm", "internal-fast") {
			t.Error("expected denied for svc-account-99")
		}
	})
}

// TestEngine_UserTypeMatch verifies user-type-based matching.
func TestEngine_UserTypeMatch(t *testing.T) {
	engine := authz.New(config.PoliciesConfig{
		Default: "deny",
		Rules: []config.PolicyRule{
			{
				Match:       config.PolicyMatch{UserTypes: []string{"premium"}},
				AllowModels: []string{"*"},
			},
		},
	})

	t.Run("premium user type allowed", func(t *testing.T) {
		p := principal(nil, nil, nil, "alice", "premium")
		if !engine.Allowed(p, "llm", "expensive-model") {
			t.Error("expected allowed for premium user")
		}
	})
	t.Run("standard user type denied", func(t *testing.T) {
		p := principal(nil, nil, nil, "bob", "standard")
		if engine.Allowed(p, "llm", "expensive-model") {
			t.Error("expected denied for standard user")
		}
	})
}

// TestEngine_NoRules verifies that with default deny and no rules, everything is denied.
func TestEngine_NoRules(t *testing.T) {
	engine := authz.New(config.PoliciesConfig{Default: "deny"})
	p := principal([]string{"admin"}, []string{"superuser"}, nil, "root", "sa")
	if engine.Allowed(p, "llm", "any-model") {
		t.Error("expected denied when no rules are configured")
	}
	if engine.Allowed(nil, "llm", "any-model") {
		t.Error("expected denied for nil principal when no rules configured")
	}
}

// ── Evaluate tests ────────────────────────────────────────────────────────────

// TestEngine_Evaluate_ReturnsLimitsFromGrantingRule verifies that Evaluate
// surfaces the granting rule's Limits block (or nil when absent).
func TestEngine_Evaluate_ReturnsLimitsFromGrantingRule(t *testing.T) {
	limits := &config.RateLimitConfig{Rate: 10, Period: "1m", TokenRate: 5000, TokenPeriod: "1h"}

	engine := authz.New(config.PoliciesConfig{
		Default: "deny",
		Rules: []config.PolicyRule{
			// Rule 0: group "limited" → model "chat-*", carries limits.
			{
				Match:       config.PolicyMatch{Groups: []string{"limited"}},
				AllowModels: []string{"chat-*"},
				Limits:      limits,
			},
			// Rule 1: group "open" → model "chat-*", no limits block.
			{
				Match:       config.PolicyMatch{Groups: []string{"open"}},
				AllowModels: []string{"chat-*"},
				Limits:      nil,
			},
		},
	})

	t.Run("granting rule with limits returns those limits", func(t *testing.T) {
		p := principal([]string{"limited"}, nil, nil, "alice", "user")
		d := engine.Evaluate(p, "llm", "chat-fast")
		if !d.Allowed {
			t.Fatal("expected allowed")
		}
		if d.Limits == nil {
			t.Fatal("expected non-nil Limits")
		}
		if d.Limits.Rate != 10 {
			t.Errorf("expected Rate=10, got %d", d.Limits.Rate)
		}
		if d.Limits.TokenRate != 5000 {
			t.Errorf("expected TokenRate=5000, got %d", d.Limits.TokenRate)
		}
	})

	t.Run("granting rule without limits returns nil Limits", func(t *testing.T) {
		p := principal([]string{"open"}, nil, nil, "bob", "user")
		d := engine.Evaluate(p, "llm", "chat-fast")
		if !d.Allowed {
			t.Fatal("expected allowed")
		}
		if d.Limits != nil {
			t.Errorf("expected nil Limits for rule without limits block, got %+v", d.Limits)
		}
	})

	t.Run("no granting rule returns Allowed=false", func(t *testing.T) {
		p := principal([]string{"unknown"}, nil, nil, "carol", "user")
		d := engine.Evaluate(p, "llm", "chat-fast")
		if d.Allowed {
			t.Fatal("expected denied")
		}
		if d.Limits != nil {
			t.Errorf("expected nil Limits on denied decision, got %+v", d.Limits)
		}
	})
}

// TestEngine_Evaluate_DefaultAllow_NilLimits verifies that Default="allow"
// returns Allowed=true with nil Limits.
func TestEngine_Evaluate_DefaultAllow_NilLimits(t *testing.T) {
	engine := authz.New(config.PoliciesConfig{Default: "allow"})
	p := principal(nil, nil, nil, "anyone", "user")
	d := engine.Evaluate(p, "llm", "any-model")
	if !d.Allowed {
		t.Fatal("default allow: expected Allowed=true")
	}
	if d.Limits != nil {
		t.Errorf("default allow: expected nil Limits, got %+v", d.Limits)
	}
}

// TestEngine_Evaluate_FirstMatchingRuleWins verifies that Evaluate returns the
// FIRST granting rule's limits even when a later rule also matches.
func TestEngine_Evaluate_FirstMatchingRuleWins(t *testing.T) {
	firstLimits := &config.RateLimitConfig{Rate: 5, Period: "1m"}
	secondLimits := &config.RateLimitConfig{Rate: 100, Period: "1h"}

	engine := authz.New(config.PoliciesConfig{
		Default: "deny",
		Rules: []config.PolicyRule{
			{
				Match:       config.PolicyMatch{Groups: []string{"g"}},
				AllowModels: []string{"m"},
				Limits:      firstLimits,
			},
			{
				Match:       config.PolicyMatch{Groups: []string{"g"}},
				AllowModels: []string{"m"},
				Limits:      secondLimits,
			},
		},
	})

	p := principal([]string{"g"}, nil, nil, "u", "user")
	d := engine.Evaluate(p, "svc", "m")
	if !d.Allowed {
		t.Fatal("expected allowed")
	}
	if d.Limits == nil || d.Limits.Rate != 5 {
		t.Errorf("expected first rule's Limits (Rate=5), got %+v", d.Limits)
	}
}
