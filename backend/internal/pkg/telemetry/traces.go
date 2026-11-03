package telemetry

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type tracingState struct {
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

var active atomic.Pointer[tracingState]

// TracingEnabled reports whether trace export is initialized.
func TracingEnabled() bool {
	return active.Load() != nil
}

func installTracing(tracer trace.Tracer, propagator propagation.TextMapPropagator) {
	active.Store(&tracingState{tracer: tracer, propagator: propagator})
}

func startTraces(ctx context.Context, cfg exporterConfig, res *resource.Resource) (func(context.Context) error, error) {
	exporter, err := newTraceExporter(ctx, cfg.protocol)
	if err != nil {
		return nil, fmt.Errorf("create otlp trace exporter: %w", err)
	}
	// The SDK reads OTEL_TRACES_SAMPLER(_ARG) and OTEL_BSP_* itself.
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	installTracing(
		provider.Tracer(instrumentationName),
		propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	)
	return func(ctx context.Context) error {
		active.Store(nil)
		return provider.Shutdown(ctx)
	}, nil
}

func newTraceExporter(ctx context.Context, protocol string) (sdktrace.SpanExporter, error) {
	// Both exporters read endpoint, headers, TLS, compression and timeout
	// from the standard OTEL_EXPORTER_OTLP_* variables.
	if protocol == protocolGRPC {
		return otlptracegrpc.New(ctx)
	}
	return otlptracehttp.New(ctx)
}
