// Package telemetry bootstraps the OpenTelemetry SDK.
// Call Init() once at startup and defer the returned shutdown function.
// After Init(), each package acquires its own tracer via otel.Tracer(name) —
// no global variable is exposed here, which keeps the package free of
// hidden coupling and makes components independently testable.
package telemetry

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Init bootstraps the OTel TracerProvider.
// After this call, otel.Tracer(name) returns a live tracer backed by the
// configured OTLP-HTTP exporter.
func Init(ctx context.Context, serviceName, endpoint, publicKey, secretKey string) (shutdown func(context.Context) error, err error) {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(endpoint),
		otlptracehttp.WithInsecure(),
	}
	if publicKey != "" && secretKey != "" {
		encoded := base64.StdEncoding.EncodeToString([]byte(publicKey + ":" + secretKey))
		opts = append(opts, otlptracehttp.WithHeaders(map[string]string{
			"Authorization": "Basic " + encoded,
		}))
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),
		resource.WithProcess(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(2*time.Second),
			sdktrace.WithMaxExportBatchSize(512),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	// Point the factory at the OTel adapter so all subsequent NewTracer() calls
	// return a live exporting tracer.
	SetFactory(otelFactory)

	return func(ctx context.Context) error {
		shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return tp.Shutdown(shutdownCtx)
	}, nil
}

