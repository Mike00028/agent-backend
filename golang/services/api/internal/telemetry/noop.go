package telemetry

import "context"

// noopTracer and noopSpan are the zero-value implementations used when no
// factory has been configured (e.g. unit tests that don't need traces).

type noopTracer struct{}

func (n *noopTracer) Start(ctx context.Context, _ string, _ ...Attr) (context.Context, Span) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) End()                {}
func (noopSpan) SetAttr(_ ...Attr)   {}
func (noopSpan) RecordError(_ error) {}
func (noopSpan) SetError(_ string)   {}
