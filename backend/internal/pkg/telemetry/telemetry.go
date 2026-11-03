// Package telemetry integrates OpenTelemetry traces and logs (OTLP export)
// into sub2api.
//
// It is opt-in and configured only through the standard OTEL_* environment
// variables, so it does not extend the config schema. Each signal is enabled
// when an OTLP endpoint applies to it:
//
//   - Traces: OTEL_EXPORTER_OTLP_TRACES_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT;
//     turned off by OTEL_TRACES_EXPORTER=none.
//   - Logs: OTEL_EXPORTER_OTLP_LOGS_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT;
//     turned off by OTEL_LOGS_EXPORTER=none.
//   - OTEL_SDK_DISABLED=true turns off both.
//   - OTEL_EXPORTER_OTLP_[TRACES_|LOGS_]PROTOCOL selects http/protobuf
//     (default) or grpc.
//   - Endpoint, headers, TLS, timeout, sampler (OTEL_TRACES_SAMPLER*),
//     batching (OTEL_BSP_*, OTEL_BLRP_*) and resource (OTEL_SERVICE_NAME,
//     OTEL_RESOURCE_ATTRIBUTES) are read by the OpenTelemetry SDK itself.
//
// Trace context is extracted from incoming requests (W3C traceparent), so
// spans join the caller's trace. It is never injected into requests sent to
// upstream providers. Logs written through the request-scoped logger carry
// the request's trace context, so the backend links them to the trace.
//
// The package deliberately does not install the OpenTelemetry globals
// (TracerProvider, LoggerProvider, TextMapPropagator). Some dependencies carry
// their own OpenTelemetry instrumentation (for example otelhttp); leaving the
// globals as no-ops keeps them from emitting telemetry or injecting trace
// headers into their outbound requests.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.uber.org/zap"
)

const instrumentationName = "github.com/Wei-Shaw/sub2api/internal/pkg/telemetry"

// defaultServiceName is the service.name reported to the backend unless
// OTEL_SERVICE_NAME overrides it. It is intentionally independent of
// log.service_name.
const defaultServiceName = "ai-gateway"

// envAttrKey is the legacy deployment.environment key rather than the
// current semconv deployment.environment.name: it is the key the other
// services in our SigNoz report, so environment filters match across them.
const envAttrKey = attribute.Key("deployment.environment")

// selfLoggerName names the logger used for the SDK's own error reports. Its
// entries are kept out of the OTLP log export (see logs.go), so a failing
// exporter cannot feed its errors back into itself.
const selfLoggerName = "opentelemetry"

const (
	protocolHTTPProtobuf = "http/protobuf"
	protocolGRPC         = "grpc"
)

// Options provides resource defaults. OTEL_RESOURCE_ATTRIBUTES takes
// precedence over them.
type Options struct {
	ServiceVersion string
	Environment    string
}

// Init configures traces and logs export from the environment. It returns a
// shutdown function that flushes buffered telemetry; the function is never
// nil. When no signal is enabled by the environment, Init is a no-op. On
// error nothing stays enabled.
func Init(ctx context.Context, opts Options) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	getenv := os.Getenv

	if strings.EqualFold(strings.TrimSpace(getenv("OTEL_SDK_DISABLED")), "true") {
		return noop, nil
	}
	tracesCfg, err := signalConfigFromEnv(getenv, signalTraces)
	if err != nil {
		return noop, err
	}
	logsCfg, err := signalConfigFromEnv(getenv, signalLogs)
	if err != nil {
		return noop, err
	}
	if !tracesCfg.enabled && !logsCfg.enabled {
		return noop, nil
	}

	res, err := newResource(ctx, opts)
	if err != nil {
		return noop, fmt.Errorf("build otel resource: %w", err)
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.L().Named(selfLoggerName).Warn("opentelemetry error", zap.String("component", "telemetry"), zap.Error(err))
	}))

	var stops []func(context.Context) error
	shutdown := func(ctx context.Context) error {
		var errs []error
		for i := len(stops) - 1; i >= 0; i-- {
			errs = append(errs, stops[i](ctx))
		}
		return errors.Join(errs...)
	}

	if tracesCfg.enabled {
		stop, err := startTraces(ctx, tracesCfg, res)
		if err != nil {
			_ = shutdown(ctx)
			return noop, err
		}
		stops = append(stops, stop)
	}
	if logsCfg.enabled {
		stop, err := startLogs(ctx, logsCfg, res)
		if err != nil {
			_ = shutdown(ctx)
			return noop, err
		}
		stops = append(stops, stop)
	}

	logger.L().Info("opentelemetry enabled",
		zap.String("component", "telemetry"),
		zap.Bool("traces", tracesCfg.enabled),
		zap.String("traces_endpoint", tracesCfg.endpointForLog),
		zap.Bool("logs", logsCfg.enabled),
		zap.String("logs_endpoint", logsCfg.endpointForLog),
	)
	return shutdown, nil
}

type signal struct {
	envName string // the signal part of OTEL_* variable names
}

var (
	signalTraces = signal{envName: "TRACES"}
	signalLogs   = signal{envName: "LOGS"}
)

type exporterConfig struct {
	enabled        bool
	protocol       string
	endpointForLog string
}

func signalConfigFromEnv(getenv func(string) string, sig signal) (exporterConfig, error) {
	env := func(key string) string { return strings.TrimSpace(getenv(key)) }

	exporterVar := "OTEL_" + sig.envName + "_EXPORTER"
	switch exporter := strings.ToLower(env(exporterVar)); exporter {
	case "", "otlp":
	case "none":
		return exporterConfig{}, nil
	default:
		return exporterConfig{}, fmt.Errorf("unsupported %s %q: only otlp and none are supported", exporterVar, exporter)
	}

	endpoint := env("OTEL_EXPORTER_OTLP_" + sig.envName + "_ENDPOINT")
	if endpoint == "" {
		endpoint = env("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint == "" {
		return exporterConfig{}, nil
	}

	protocol := env("OTEL_EXPORTER_OTLP_" + sig.envName + "_PROTOCOL")
	if protocol == "" {
		protocol = env("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch strings.ToLower(protocol) {
	case "", protocolHTTPProtobuf:
		protocol = protocolHTTPProtobuf
	case protocolGRPC:
		protocol = protocolGRPC
	default:
		return exporterConfig{}, fmt.Errorf("unsupported OTLP protocol %q: only http/protobuf and grpc are supported", protocol)
	}

	return exporterConfig{enabled: true, protocol: protocol, endpointForLog: redactEndpoint(endpoint)}, nil
}

func newResource(ctx context.Context, opts Options) (*resource.Resource, error) {
	defaults := []attribute.KeyValue{semconv.ServiceName(defaultServiceName)}
	if v := strings.TrimSpace(opts.ServiceVersion); v != "" {
		defaults = append(defaults, semconv.ServiceVersion(v))
	}
	if v := strings.TrimSpace(opts.Environment); v != "" {
		defaults = append(defaults, envAttrKey.String(v))
	}

	// Later options override earlier ones, so OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES win over the defaults above.
	res, err := resource.New(ctx,
		resource.WithAttributes(defaults...),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if errors.Is(err, resource.ErrPartialResource) {
		// A detector could not read everything (e.g. host name); the
		// remaining attributes are still usable.
		logger.L().Warn("opentelemetry resource partially detected", zap.String("component", "telemetry"), zap.Error(err))
		err = nil
	}
	return res, err
}

// redactEndpoint drops userinfo and query from the endpoint before logging.
func redactEndpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	if strings.ContainsAny(endpoint, "@?") {
		return "(redacted)"
	}
	return endpoint
}
