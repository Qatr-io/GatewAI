// Package service holds the inference service registry.
// Adding a new inference type (e.g. "translation") requires only a new entry
// in config.yaml — no Go code modification.
package service

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gatewai/gateway/internal/config"
)

// GuardrailsStage is the resolved guardrails configuration for a single pipeline stage.
type GuardrailsStage struct {
	Enabled bool
	Checks  []string // resolved group names to run
	Action  string   // "block" | "redact" | "flag"
}

// GuardrailsSpec holds resolved guardrails for both the input and output stages.
type GuardrailsSpec struct {
	Input  GuardrailsStage
	Output GuardrailsStage
}

// Def describes a registered inference service type.
type Def struct {
	Type          string
	Model         string
	AcceptedExts  map[string]struct{} // empty = accept any extension
	MaxFileSizeMB int64
	// SupportsAsync is true when the service is configured for async file-upload jobs
	// (i.e. AcceptedExts is explicitly set in the config).
	SupportsAsync     bool
	MaxConcurrentSync int // max simultaneous sync calls; 0 = unlimited
	// PriorityReservedSync reserves this many MaxConcurrentSync slots exclusively
	// for requests carrying server.priority_header. 0 = no reservation.
	PriorityReservedSync int

	// Sync / OpenAI-compatible mode (optional).
	InferenceURL     string              // primary backend URL (derived from Backends; kept for compatibility)
	Backends         []Backend           // ordered list of backends; always non-empty when InferenceURL != ""
	Operations       map[string][]string // operation name → URL paths (all indexed; first used for async)
	InferenceHeaders map[string]string   // headers injected on every sync-direct proxy request to the backend
	Provider         string
	BackendModel     string // real model name sent to backend; empty = use Model (the alias)
	ResponseCacheTTL time.Duration
	Retries          int            // additional full backend-cycle attempts on network error or 5xx (sync-direct only)
	Guardrails       GuardrailsSpec // resolved guardrails configuration
	HealthCheck      config.ServiceHealthConfig
	Deprecated       bool // informational: surfaced in /v1/models and OpenAPI specs
}

// OperationPath returns the first path for the given operation name.
// If op is empty and exactly one operation is configured, that operation is used.
func (d *Def) OperationPath(op string) (string, error) {
	if len(d.Operations) == 0 {
		return "", nil
	}
	if op == "" {
		if len(d.Operations) == 1 {
			for _, paths := range d.Operations {
				if len(paths) > 0 {
					return paths[0], nil
				}
			}
		}
		available := make([]string, 0, len(d.Operations))
		for name := range d.Operations {
			available = append(available, name)
		}
		return "", fmt.Errorf("model %q has multiple operations, specify one via -F operation=... (available: %v)", d.Model, available)
	}
	paths, ok := d.Operations[op]
	if !ok {
		available := make([]string, 0, len(d.Operations))
		for name := range d.Operations {
			available = append(available, name)
		}
		return "", fmt.Errorf("operation %q not found for model %q (available: %v)", op, d.Model, available)
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("operation %q has no paths configured for model %q", op, d.Model)
	}
	return paths[0], nil
}

// IsLLM reports whether this service uses the LLM proxy handler.
func (d *Def) IsLLM() bool {
	return d.Provider != ""
}

// pathPattern supports openai_paths entries that contain a {model} placeholder,
// e.g. "/v2/models/{model}/infer" or "/v1/models/{model}:predict".
// The placeholder can be an entire path segment or embedded within one
// (prefix/suffix around {model} in the same segment are matched literally).
type pathPattern struct {
	pattern     string
	segments    []string        // pattern split by "/"
	defsByModel map[string]*Def // model name → Def
}

// match returns the extracted model name if urlPath matches the pattern.
func (p *pathPattern) match(urlPath string) (model string, ok bool) {
	urlSegs := strings.Split(urlPath, "/")
	if len(urlSegs) != len(p.segments) {
		return "", false
	}
	for i, patSeg := range p.segments {
		if strings.Contains(patSeg, "{model}") {
			m, matched := matchModelSegment(patSeg, urlSegs[i])
			if !matched || m == "" {
				return "", false
			}
			model = m
		} else if urlSegs[i] != patSeg {
			return "", false
		}
	}
	return model, model != ""
}

// matchModelSegment extracts the model name from a URL segment given a pattern
// segment that contains exactly one {model} placeholder.
// Examples:
//
//	pattern "{model}"         + actual "whisper-large-v3"         → "whisper-large-v3"
//	pattern "{model}:predict" + actual "whisper-large-v3:predict" → "whisper-large-v3"
func matchModelSegment(pattern, actual string) (model string, ok bool) {
	parts := strings.SplitN(pattern, "{model}", 2)
	prefix, suffix := parts[0], parts[1]
	if !strings.HasPrefix(actual, prefix) || !strings.HasSuffix(actual, suffix) {
		return "", false
	}
	// Guard against overlap when prefix+suffix is longer than actual.
	if len(actual) < len(prefix)+len(suffix) {
		return "", false
	}
	return actual[len(prefix) : len(actual)-len(suffix)], true
}

// wildcardRoute holds services registered under a path prefix wildcard (e.g. "/v1/*").
// It matches any request path that starts with prefix (the path with "*" stripped).
type wildcardRoute struct {
	pattern string // original pattern, e.g. "/v1/*"
	prefix  string // prefix to match against, e.g. "/v1/"
	byModel map[string]*Def
	deflt   *Def // default def when model is empty or not found by name
}

// Registry maps (service_type, model) pairs to their runtime definitions.
type Registry struct {
	byTypeModel   map[string]map[string]*Def // type → model → Def
	defaultByType map[string]*Def            // type → default Def (when default: true in config)
	bySync        map[string]map[string]*Def // exact openai_path → model → Def
	defaultByPath map[string]*Def            // exact openai_path → default Def
	byPattern     []*pathPattern             // pattern paths containing {model}
	byWildcard    []*wildcardRoute           // wildcard prefix paths ending with /*
}

// resolveStage converts a check list and action string into a GuardrailsStage.
// When checks are non-empty, the stage is enabled with those checks and action
// (defaulting to "block" when action is empty). Otherwise the zero (disabled)
// stage is returned.
func resolveStage(checks []string, action string) GuardrailsStage {
	if len(checks) > 0 {
		if action == "" {
			action = "block"
		}
		return GuardrailsStage{Enabled: true, Checks: checks, Action: action}
	}
	return GuardrailsStage{}
}

// resolveGuardrails converts a config.GuardrailsConfig into a runtime GuardrailsSpec.
// The top-level Checks/Action fields map to the Input stage. The Output sub-key
// maps to the Output stage. Both default to disabled when not configured.
func resolveGuardrails(cfg config.GuardrailsConfig) GuardrailsSpec {
	input := resolveStage(cfg.Checks, cfg.Action)
	var output GuardrailsStage
	if cfg.Output != nil {
		output = resolveStage(cfg.Output.Checks, cfg.Output.Action)
	}
	return GuardrailsSpec{Input: input, Output: output}
}

func NewRegistry(cfgs []config.ServiceConfig) *Registry {
	r := &Registry{
		byTypeModel:   make(map[string]map[string]*Def, len(cfgs)),
		defaultByType: make(map[string]*Def),
		bySync:        make(map[string]map[string]*Def),
		defaultByPath: make(map[string]*Def),
	}
	for _, cfg := range cfgs {
		exts := make(map[string]struct{}, len(cfg.AcceptedExts))
		for _, ext := range cfg.AcceptedExts {
			exts[strings.ToLower(ext)] = struct{}{}
		}
		backends := normalizeBackends(cfg.Backends, cfg.InferenceURL)
		primaryURL := cfg.InferenceURL
		for _, b := range backends {
			if b.Weight > 0 {
				primaryURL = b.URL
				break
			}
		}
		if primaryURL == "" && len(backends) > 0 {
			primaryURL = backends[0].URL
		}
		def := &Def{
			Type:                 cfg.Type,
			Model:                cfg.Model,
			AcceptedExts:         exts,
			MaxFileSizeMB:        cfg.MaxFileSizeMB,
			SupportsAsync:        len(cfg.AcceptedExts) > 0,
			MaxConcurrentSync:    cfg.MaxConcurrentSync,
			PriorityReservedSync: cfg.PriorityReservedSync,
			InferenceURL:         primaryURL,
			Backends:             backends,
			Operations:           cfg.Operations,
			InferenceHeaders:     cfg.InferenceHeaders,
			Provider:             cfg.Provider,
			BackendModel:         cfg.BackendModel,
			ResponseCacheTTL:     time.Duration(cfg.ResponseCacheTTL) * time.Second,
			Retries:              cfg.Retries,
			Guardrails:           resolveGuardrails(cfg.Guardrails),
			HealthCheck:          cfg.Health,
			Deprecated:        cfg.Deprecated,
		}

		if r.byTypeModel[cfg.Type] == nil {
			r.byTypeModel[cfg.Type] = make(map[string]*Def)
		}
		r.byTypeModel[cfg.Type][cfg.Model] = def

		if cfg.Default {
			r.defaultByType[cfg.Type] = def
		}

		// Build the sync routing index — one entry per configured path across all operations.
		// Index when a direct proxy backend is configured.
		hasBackend := cfg.InferenceURL != "" || len(cfg.Backends) > 0
		if cfg.Model != "" && hasBackend {
			for _, paths := range cfg.Operations {
				for _, path := range paths {
					if path == "" {
						continue
					}
					if strings.Contains(path, "{model}") {
						// Pattern path — model is embedded in the URL.
						r.indexPattern(path, cfg.Model, def)
					} else if strings.HasSuffix(path, "/*") {
						// Wildcard prefix path — matches any sub-path under the prefix.
						r.indexWildcard(path, cfg.Model, def, cfg.Default)
					} else {
						// Exact path — model is expected in the request body.
						if r.bySync[path] == nil {
							r.bySync[path] = make(map[string]*Def)
						}
						r.bySync[path][cfg.Model] = def
						if cfg.Default {
							r.defaultByPath[path] = def
						}
					}
				}
			}
		}
	}
	return r
}

// normalizeBackends converts the config backends list to the runtime Backend slice.
// When cfgBackends is empty but legacyURL is set, a single weight=1 backend is synthesized.
func normalizeBackends(cfgBackends []config.BackendConfig, legacyURL string) []Backend {
	if len(cfgBackends) > 0 {
		out := make([]Backend, len(cfgBackends))
		for i, b := range cfgBackends {
			out[i] = Backend{URL: b.URL, Weight: b.Weight, Headers: b.Headers, Model: b.Model}
		}
		return out
	}
	if legacyURL != "" {
		return []Backend{{URL: legacyURL, Weight: 1}}
	}
	return nil
}

// indexPattern adds def to the pattern index, merging into an existing pattern
// entry when the same path template is shared by multiple service configs.
func (r *Registry) indexPattern(pattern, model string, def *Def) {
	for _, p := range r.byPattern {
		if p.pattern == pattern {
			p.defsByModel[model] = def
			return
		}
	}
	r.byPattern = append(r.byPattern, &pathPattern{
		pattern:     pattern,
		segments:    strings.Split(pattern, "/"),
		defsByModel: map[string]*Def{model: def},
	})
}

// indexWildcard adds def to the wildcard index under the given pattern (e.g. "/v1/*").
// Multiple service configs may share the same wildcard pattern (different models).
func (r *Registry) indexWildcard(pattern, model string, def *Def, isDefault bool) {
	prefix := strings.TrimSuffix(pattern, "*") // "/v1/*" → "/v1/"
	for _, w := range r.byWildcard {
		if w.pattern == pattern {
			w.byModel[model] = def
			if isDefault {
				w.deflt = def
			}
			return
		}
	}
	w := &wildcardRoute{
		pattern: pattern,
		prefix:  prefix,
		byModel: map[string]*Def{model: def},
	}
	if isDefault {
		w.deflt = def
	}
	r.byWildcard = append(r.byWildcard, w)
}

// RouteAsync returns the Def for the given (service_type, model) pair.
// Resolution order when model is empty:
//  1. Single model configured for the type → auto-selected.
//  2. A model marked default: true for the type → used as fallback.
//  3. Error listing available models.
func (r *Registry) RouteAsync(serviceType, model string) (*Def, error) {
	models, ok := r.byTypeModel[serviceType]
	if !ok {
		return nil, fmt.Errorf("unknown service type %q", serviceType)
	}
	if model == "" {
		if len(models) == 1 {
			for _, d := range models {
				return d, nil
			}
		}
		if d, ok := r.defaultByType[serviceType]; ok {
			return d, nil
		}
		available := make([]string, 0, len(models))
		for m := range models {
			available = append(available, m)
		}
		return nil, fmt.Errorf("service type %q has multiple models, specify one via -F model=... (available: %v)", serviceType, available)
	}
	d, ok := models[model]
	if !ok {
		available := make([]string, 0, len(models))
		for m := range models {
			available = append(available, m)
		}
		return nil, fmt.Errorf("model %q not found for service type %q (available: %v)", model, serviceType, available)
	}
	return d, nil
}

// RouteSync returns the service Def for the incoming request.
//
// Lookup order:
//  1. Exact path match — model from request body.
//     If model is empty: single registered model, then default model, then error.
//  2. Pattern path match — model extracted from URL.
//  3. Wildcard prefix match (e.g. "/v1/*") — model from request body.
func (r *Registry) RouteSync(openaiPath, model string) (*Def, error) {
	// 1. Exact path.
	if models, ok := r.bySync[openaiPath]; ok {
		if d, ok := models[model]; ok {
			return d, nil
		}
		if model == "" {
			if len(models) == 1 {
				for _, d := range models {
					return d, nil
				}
			}
			if d, ok := r.defaultByPath[openaiPath]; ok {
				return d, nil
			}
		}
		available := make([]string, 0, len(models))
		for m := range models {
			available = append(available, m)
		}
		return nil, fmt.Errorf("no service for path %q with model %q (available: %v)", openaiPath, model, available)
	}

	// 2. Pattern path — model extracted from URL.
	for _, p := range r.byPattern {
		extracted, ok := p.match(openaiPath)
		if !ok {
			continue
		}
		if d, ok := p.defsByModel[extracted]; ok {
			return d, nil
		}
		available := make([]string, 0, len(p.defsByModel))
		for m := range p.defsByModel {
			available = append(available, m)
		}
		return nil, fmt.Errorf("path %q matched pattern %q but model %q is not registered (available: %v)",
			openaiPath, p.pattern, extracted, available)
	}

	// 3. Wildcard prefix — any sub-path under the configured prefix.
	for _, w := range r.byWildcard {
		if !strings.HasPrefix(openaiPath, w.prefix) {
			continue
		}
		if d, ok := w.byModel[model]; ok {
			return d, nil
		}
		if model == "" {
			if len(w.byModel) == 1 {
				for _, d := range w.byModel {
					return d, nil
				}
			}
			if w.deflt != nil {
				return w.deflt, nil
			}
		}
		available := make([]string, 0, len(w.byModel))
		for m := range w.byModel {
			available = append(available, m)
		}
		return nil, fmt.Errorf("no service for path %q (matched wildcard %q) with model %q (available: %v)",
			openaiPath, w.pattern, model, available)
	}

	return nil, fmt.Errorf("no sync service configured for path %q", openaiPath)
}

// HasSyncServices reports whether at least one service has sync mode configured.
func (r *Registry) HasSyncServices() bool {
	return len(r.bySync) > 0 || len(r.byPattern) > 0 || len(r.byWildcard) > 0
}

// SyncPaths returns the unique OpenAI paths/patterns that have a sync backend.
// Wildcard paths (e.g. "/v1/*") are included as-is and registered directly with chi.
func (r *Registry) SyncPaths() []string {
	paths := make([]string, 0, len(r.bySync)+len(r.byPattern)+len(r.byWildcard))
	for p := range r.bySync {
		paths = append(paths, p)
	}
	for _, p := range r.byPattern {
		paths = append(paths, p.pattern)
	}
	for _, w := range r.byWildcard {
		paths = append(paths, w.pattern)
	}
	return paths
}

// SyncPathPrefixes returns unique first-level path prefixes (e.g. "/v1", "/v2")
// across all registered sync paths. Used to register chi wildcard routes.
func (r *Registry) SyncPathPrefixes() []string {
	seen := make(map[string]struct{})
	for path := range r.bySync {
		seen[pathPrefix(path)] = struct{}{}
	}
	for _, p := range r.byPattern {
		seen[pathPrefix(p.pattern)] = struct{}{}
	}
	for _, w := range r.byWildcard {
		seen[pathPrefix(w.pattern)] = struct{}{}
	}
	prefixes := make([]string, 0, len(seen))
	for prefix := range seen {
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// pathPrefix returns the first non-empty path segment with a leading slash,
// e.g. "/v1/audio/transcriptions" → "/v1", "/v2/models/{model}/infer" → "/v2".
func pathPrefix(path string) string {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	return "/" + parts[0]
}

// MaxFileSizeForType returns the largest MaxFileSizeMB across all models for
// the given service type. Used to set the body read limit before form parsing,
// before the specific model is known.
func (r *Registry) MaxFileSizeForType(serviceType string) (int64, error) {
	models, ok := r.byTypeModel[serviceType]
	if !ok {
		return 0, fmt.Errorf("unknown service type %q", serviceType)
	}
	var max int64
	for _, d := range models {
		if d.MaxFileSizeMB > max {
			max = d.MaxFileSizeMB
		}
	}
	return max, nil
}

// ValidateFileDef checks that the file's extension is accepted by the given service def.
func (r *Registry) ValidateFileDef(def *Def, filename string) error {
	if len(def.AcceptedExts) == 0 {
		return nil // no restriction configured
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if _, ok := def.AcceptedExts[ext]; !ok {
		accepted := make([]string, 0, len(def.AcceptedExts))
		for e := range def.AcceptedExts {
			accepted = append(accepted, e)
		}
		return fmt.Errorf("extension %q not accepted for model %q (accepted: %v)", ext, def.Model, accepted)
	}
	return nil
}

// ModelsForType returns the model names registered for the given service type
// (excluding services with no model alias). Used to enumerate per-model
// token-quota breakdowns in usage reporting.
func (r *Registry) ModelsForType(serviceType string) []string {
	models := make([]string, 0, len(r.byTypeModel[serviceType]))
	for m := range r.byTypeModel[serviceType] {
		if m != "" {
			models = append(models, m)
		}
	}
	return models
}

// Types returns all registered service type names (unique).
func (r *Registry) Types() []string {
	types := make([]string, 0, len(r.byTypeModel))
	for t := range r.byTypeModel {
		types = append(types, t)
	}
	return types
}

// Models returns all service definitions that expose an OpenAI-compatible model
// (i.e. have a non-empty Model field). Used for the GET /v1/models endpoint.
func (r *Registry) Models() []*Def {
	var defs []*Def
	for _, models := range r.byTypeModel {
		for _, d := range models {
			if d.Model != "" {
				defs = append(defs, d)
			}
		}
	}
	return defs
}

// All returns all service definitions.
func (r *Registry) All() []*Def {
	var defs []*Def
	for _, models := range r.byTypeModel {
		for _, d := range models {
			defs = append(defs, d)
		}
	}
	return defs
}
