package guardrails

// Observer receives model-detector telemetry. It exists so this package stays a
// leaf (no metrics import): the caller wires an adapter that forwards to the
// metrics package. The default is a no-op.
type Observer interface {
	// ObserveModelLatency records a detector call's duration in seconds.
	ObserveModelLatency(detector string, seconds float64)
	// IncModelError records a detector failure by reason
	// ("timeout"|"unreachable"|"bad_response").
	IncModelError(detector, reason string)
}

type nopObserver struct{}

func (nopObserver) ObserveModelLatency(string, float64) {}
func (nopObserver) IncModelError(string, string)        {}

// observer is the package-level sink, replaceable via SetObserver at startup.
var observer Observer = nopObserver{}

// SetObserver installs the telemetry sink for model detectors. Call once during
// startup (not safe to race with in-flight detector calls).
func SetObserver(o Observer) {
	if o == nil {
		o = nopObserver{}
	}
	observer = o
}
