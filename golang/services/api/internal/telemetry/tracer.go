// Package telemetry defines the tracing abstractions used by this service.
//
// All instrumented packages import only this file — they never depend on a
// specific tracing SDK (OTel, Datadog, Zipkin, …).
//
// The concrete backend is wired once in main.go:
//
//	telemetry.Init(...)           // default: OTel → OTLP-HTTP
//	telemetry.SetFactory(ddFact)  // or: swap to Datadog, Honeycomb, etc.
package telemetry

import "context"

// ── Attribute ─────────────────────────────────────────────────────────────────

// Attr is a key/value pair attached to a span.
// Values must be one of: string, int, int64, float64, bool.
type Attr struct {
	Key   string
	Value any
}

// Convenience constructors so callers stay concise and import-free.
func StringAttr(k, v string) Attr         { return Attr{k, v} }
func IntAttr(k string, v int) Attr        { return Attr{k, v} }
func Int64Attr(k string, v int64) Attr    { return Attr{k, v} }
func Float64Attr(k string, v float64) Attr { return Attr{k, v} }
func BoolAttr(k string, v bool) Attr      { return Attr{k, v} }

// ── Span ──────────────────────────────────────────────────────────────────────

// Span represents an in-flight trace event.
// Implementations must be safe to call concurrently.
type Span interface {
	// End marks the span complete. Always defer this.
	End()
	// SetAttr attaches key/value attributes.
	SetAttr(attrs ...Attr)
	// RecordError marks the span as failed and attaches the error.
	RecordError(err error)
	// SetError sets the span status to Error with a message.
	SetError(msg string)
}

// ── Tracer ────────────────────────────────────────────────────────────────────

// Tracer creates spans. Components depend on this interface, not on any SDK.
type Tracer interface {
	// Start creates a child span. The returned context carries the span so
	// further Start calls become children automatically.
	Start(ctx context.Context, name string, attrs ...Attr) (context.Context, Span)
}

// ── Factory ───────────────────────────────────────────────────────────────────

// tracerFactory is the function used by NewTracer.
// Default: OTel adapter (set by Init). Override with SetFactory.
var tracerFactory func(name string) Tracer = func(name string) Tracer {
	return &noopTracer{}
}

// NewTracer returns a named Tracer backed by whichever factory is active.
// Each package calls this once at package-init or struct-construction time.
func NewTracer(name string) Tracer {
	return tracerFactory(name)
}

// SetFactory replaces the tracer factory globally.
// Call this in main.go before any component is constructed to swap the backend:
//
//	telemetry.SetFactory(func(name string) telemetry.Tracer {
//	    return myDatadogAdapter(name)
//	})
func SetFactory(f func(name string) Tracer) {
	tracerFactory = f
}
