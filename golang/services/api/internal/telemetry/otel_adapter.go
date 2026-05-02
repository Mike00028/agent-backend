package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// ── OTel adapter ──────────────────────────────────────────────────────────────

// otelTracer wraps an OTel trace.Tracer to satisfy telemetry.Tracer.
type otelTracer struct {
	t oteltrace.Tracer
}

func (o *otelTracer) Start(ctx context.Context, name string, attrs ...Attr) (context.Context, Span) {
	ctx, span := o.t.Start(ctx, name)
	if len(attrs) > 0 {
		span.SetAttributes(toOtelAttrs(attrs)...)
	}
	return ctx, &otelSpan{span}
}

// otelSpan wraps an OTel trace.Span to satisfy telemetry.Span.
type otelSpan struct {
	s oteltrace.Span
}

func (s *otelSpan) End()                  { s.s.End() }
func (s *otelSpan) RecordError(err error) { s.s.RecordError(err) }
func (s *otelSpan) SetError(msg string)   { s.s.SetStatus(codes.Error, msg) }
func (s *otelSpan) SetAttr(attrs ...Attr) { s.s.SetAttributes(toOtelAttrs(attrs)...) }

func toOtelAttrs(attrs []Attr) []attribute.KeyValue {
	kv := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		switch v := a.Value.(type) {
		case string:
			kv = append(kv, attribute.String(a.Key, v))
		case int:
			kv = append(kv, attribute.Int(a.Key, v))
		case int64:
			kv = append(kv, attribute.Int64(a.Key, v))
		case float64:
			kv = append(kv, attribute.Float64(a.Key, v))
		case bool:
			kv = append(kv, attribute.Bool(a.Key, v))
		}
	}
	return kv
}

// otelFactory returns a Tracer backed by the OTel global TracerProvider.
// After telemetry.Init() this is a live exporting tracer.
// Before Init() (e.g. in tests) the OTel global is a noop — no export happens.
func otelFactory(name string) Tracer {
	return &otelTracer{t: otel.Tracer(name)}
}
