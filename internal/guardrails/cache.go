package guardrails

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// VerdictCache stores model-detector findings keyed by an opaque string, so a
// repeat (or identical) payload skips the model call. It exists so this package
// stays a leaf: the caller injects a Redis-backed adapter at startup. Default is
// a no-op (caching disabled).
type VerdictCache interface {
	Get(ctx context.Context, key string) ([]Finding, bool)
	Set(ctx context.Context, key string, findings []Finding, ttl time.Duration)
}

type nopVerdictCache struct{}

func (nopVerdictCache) Get(context.Context, string) ([]Finding, bool)         { return nil, false }
func (nopVerdictCache) Set(context.Context, string, []Finding, time.Duration) {}

var verdictCache VerdictCache = nopVerdictCache{}

// SetVerdictCache installs the verdict cache. Call once during startup.
func SetVerdictCache(c VerdictCache) {
	if c == nil {
		c = nopVerdictCache{}
	}
	verdictCache = c
}

// verdictKey derives a stable cache key from the detector name and the texts it
// scans. Texts are joined with a NUL separator (which cannot appear in JSON
// string content) so distinct inputs never collide.
func verdictKey(detector string, texts []string) string {
	h := sha256.New()
	h.Write([]byte(detector))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(texts, "\x00")))
	return hex.EncodeToString(h.Sum(nil))
}
