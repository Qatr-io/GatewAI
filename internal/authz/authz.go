// Package authz implements a config-driven access policy engine.
//
// The engine operates in default-deny mode: access is granted only when an
// explicit rule matches the principal AND the (serviceType, model) pair.
// Setting Policies.Default to "allow" disables enforcement entirely.
//
// Build an Engine from a PoliciesConfig with New, then call Allowed for each
// request. The Engine is safe for concurrent use after construction.
//
// Glob matching uses path/filepath.Match; model aliases and service types must
// not contain "/" — standard shell-style patterns apply ("*", "?", "[abc]").
package authz

import (
	"path/filepath"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/config"
)

// compiledRule is a pre-parsed policy rule ready for evaluation.
type compiledRule struct {
	match             config.PolicyMatch
	allowModels       []string
	allowServiceTypes []string
}

// Engine evaluates access policies. Build with New; a nil *PoliciesConfig means
// "no enforcement" — construct the Engine only when policies are configured
// (the caller decides). Default posture is DENY (allowlist).
type Engine struct {
	defaultAllow bool
	rules        []compiledRule
}

// New constructs an Engine from cfg. The returned Engine is safe for
// concurrent use after construction.
func New(cfg config.PoliciesConfig) *Engine {
	e := &Engine{
		defaultAllow: cfg.Default == "allow",
	}
	for _, r := range cfg.Rules {
		e.rules = append(e.rules, compiledRule{
			match:             r.Match,
			allowModels:       r.AllowModels,
			allowServiceTypes: r.AllowServiceTypes,
		})
	}
	return e
}

// Allowed reports whether the principal p may use (serviceType, model).
// p may be nil (treated as anonymous: empty groups/roles/scopes/consumer/userType).
//
// Semantics:
//   - If Default == "allow" → always true (enforcement disabled via config).
//   - Otherwise (default deny): iterate rules; a rule grants iff
//     matchIdentity(p, rule.Match) AND model matches one of AllowModels (glob)
//     AND (AllowServiceTypes is empty OR serviceType matches one of its globs).
//   - AllowModels empty → the rule grants nothing (must list "*" to allow all).
func (e *Engine) Allowed(p *auth.Principal, serviceType, model string) bool {
	if e.defaultAllow {
		return true
	}
	for _, rule := range e.rules {
		if ruleGrantsRequest(p, rule, serviceType, model) {
			return true
		}
	}
	return false
}

// ruleGrantsRequest returns true when the principal satisfies the identity
// match AND the (serviceType, model) pair is covered by the rule's allow lists.
func ruleGrantsRequest(p *auth.Principal, rule compiledRule, serviceType, model string) bool {
	if !matchIdentity(p, rule.match) {
		return false
	}
	// AllowModels empty → grants nothing.
	if len(rule.allowModels) == 0 {
		return false
	}
	if !globMatchAny(rule.allowModels, model) {
		return false
	}
	// AllowServiceTypes empty → no service-type constraint.
	if len(rule.allowServiceTypes) > 0 && !globMatchAny(rule.allowServiceTypes, serviceType) {
		return false
	}
	return true
}

// globMatchAny returns true when value matches at least one of the glob patterns.
// A malformed pattern is treated as non-matching (no panic).
func globMatchAny(patterns []string, value string) bool {
	for _, pat := range patterns {
		matched, err := filepath.Match(pat, value)
		if err != nil {
			// malformed pattern → skip (treat as non-matching)
			continue
		}
		if matched {
			return true
		}
	}
	return false
}

// matchIdentity checks whether the principal satisfies all non-empty fields of
// match. An empty PolicyMatch{} matches every principal (including nil).
//
// Nil principal is treated as having no groups/roles/scopes/consumer/userType,
// so only an entirely empty Match{} can match it.
func matchIdentity(p *auth.Principal, match config.PolicyMatch) bool {
	var (
		groups   []string
		roles    []string
		scopes   []string
		consumer string
		userType string
	)
	if p != nil {
		groups = p.Groups
		roles = p.Roles
		scopes = p.Scopes
		consumer = p.Consumer
		userType = p.UserType
	}

	if len(match.Groups) > 0 && !intersects(groups, match.Groups) {
		return false
	}
	if len(match.Roles) > 0 && !intersects(roles, match.Roles) {
		return false
	}
	if len(match.Scopes) > 0 && !intersects(scopes, match.Scopes) {
		return false
	}
	if len(match.Consumers) > 0 && !contains(match.Consumers, consumer) {
		return false
	}
	if len(match.UserTypes) > 0 && !contains(match.UserTypes, userType) {
		return false
	}
	return true
}

// intersects returns true when the two slices share at least one common element.
func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// contains returns true when needle appears in haystack.
func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
