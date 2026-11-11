package telemetry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	callerTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpanID  = "00f067aa0ba902b7"
)

// installRecorder enables tracing with an in-memory recorder. Tests touching
// the package-level state must not run in parallel.
func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	installTracing(tp.Tracer(instrumentationName), propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		active.Store(nil)
		_ = tp.Shutdown(context.Background())
	})
	return rec
}

func attrValue(attrs []attribute.KeyValue, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestSignalConfigFromEnv(t *testing.T) {
	cases := []struct {
		name         string
		sig          signal
		env          map[string]string
		wantEnabled  bool
		wantProtocol string
		wantErr      bool
	}{
		{name: "no endpoint", sig: signalTraces, env: map[string]string{}},
		{name: "base endpoint enables traces with http/protobuf", sig: signalTraces, env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://signoz:4318"}, wantEnabled: true, wantProtocol: protocolHTTPProtobuf},
		{name: "base endpoint enables logs too", sig: signalLogs, env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://signoz:4318"}, wantEnabled: true, wantProtocol: protocolHTTPProtobuf},
		{name: "traces endpoint with grpc", sig: signalTraces, env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://signoz:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, wantEnabled: true, wantProtocol: protocolGRPC},
		{name: "traces-only endpoint leaves logs off", sig: signalLogs, env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://signoz:4317"}},
		{name: "signal protocol overrides general", sig: signalLogs, env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://signoz:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "grpc"}, wantEnabled: true, wantProtocol: protocolGRPC},
		{name: "traces exporter none", sig: signalTraces, env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://signoz:4318", "OTEL_TRACES_EXPORTER": "none"}},
		{name: "logs exporter none", sig: signalLogs, env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://signoz:4318", "OTEL_LOGS_EXPORTER": "none"}},
		{name: "unsupported exporter", sig: signalTraces, env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://signoz:4318", "OTEL_TRACES_EXPORTER": "zipkin"}, wantErr: true},
		{name: "unsupported protocol", sig: signalLogs, env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://signoz:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := signalConfigFromEnv(func(key string) string { return tc.env[key] }, tc.sig)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantEnabled, cfg.enabled)
			require.Equal(t, tc.wantProtocol, cfg.protocol)
		})
	}
}

func TestRedactEndpoint(t *testing.T) {
	require.Equal(t, "https://otel.example.com/v1/traces", redactEndpoint("https://user:secret@otel.example.com/v1/traces?token=x"))
	require.Equal(t, "signoz:4317", redactEndpoint("signoz:4317"))
}

func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OTEL_SDK_DISABLED", "OTEL_TRACES_EXPORTER", "OTEL_LOGS_EXPORTER",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES",
	} {
		t.Setenv(key, "")
	}
}

func TestInitDisabledWithoutEndpoint(t *testing.T) {
	clearOTelEnv(t)

	shutdown, err := Init(context.Background(), Options{})
	require.NoError(t, err)
	require.NotNil(t, shutdown)
	require.False(t, TracingEnabled())
	require.NoError(t, shutdown(context.Background()))
}

func TestInitSDKDisabledWinsOverEndpoint(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_SDK_DISABLED", "true")

	shutdown, err := Init(context.Background(), Options{})
	require.NoError(t, err)
	require.False(t, TracingEnabled())
	require.NoError(t, shutdown(context.Background()))
}

func TestInitEnablesTracesAndLogsAndShutsDown(t *testing.T) {
	var mu sync.Mutex
	received := map[string]int{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Cleanup(func() { active.Store(nil) })

	shutdown, err := Init(context.Background(), Options{ServiceVersion: "1.2.3"})
	require.NoError(t, err)
	require.True(t, TracingEnabled())
	logger.L().Info("exported through otlp")
	require.NoError(t, shutdown(context.Background()))
	require.False(t, TracingEnabled())

	mu.Lock()
	defer mu.Unlock()
	require.Positive(t, received["/v1/logs"], "logs must be exported over OTLP/HTTP")
}

func TestSamplerIsConfiguredFromEnv(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	// Sample no root traces, but follow the caller's decision.
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0")
	t.Cleanup(func() { active.Store(nil) })

	shutdown, err := Init(context.Background(), Options{})
	require.NoError(t, err)
	defer func() { require.NoError(t, shutdown(context.Background())) }()

	sampled := func(traceparent string) bool {
		var got bool
		r := newTracedRouter(func(c *gin.Context) {
			got = trace.SpanContextFromContext(c.Request.Context()).IsSampled()
			c.Status(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		if traceparent != "" {
			req.Header.Set("traceparent", traceparent)
		}
		r.ServeHTTP(httptest.NewRecorder(), req)
		return got
	}

	require.False(t, sampled(""), "root requests follow OTEL_TRACES_SAMPLER_ARG=0")
	require.True(t, sampled("00-"+callerTraceID+"-"+callerSpanID+"-01"), "a sampled caller is followed")
	require.False(t, sampled("00-"+callerTraceID+"-"+callerSpanID+"-00"), "an unsampled caller is followed")
}

func TestResourceDeploymentEnvironment(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	res, err := newResource(context.Background(), Options{Environment: "staging"})
	require.NoError(t, err)
	v, ok := res.Set().Value("deployment.environment")
	require.True(t, ok)
	require.Equal(t, "staging", v.AsString())

	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=loadtest")
	res, err = newResource(context.Background(), Options{Environment: "staging"})
	require.NoError(t, err)
	v, _ = res.Set().Value("deployment.environment")
	require.Equal(t, "loadtest", v.AsString())
}

func TestResourceServiceName(t *testing.T) {
	serviceName := func(t *testing.T) string {
		t.Helper()
		res, err := newResource(context.Background(), Options{ServiceVersion: "1.2.3", Environment: "prod"})
		require.NoError(t, err)
		v, ok := res.Set().Value("service.name")
		require.True(t, ok)
		return v.AsString()
	}

	t.Run("defaults to ai-gateway", func(t *testing.T) {
		t.Setenv("OTEL_SERVICE_NAME", "")
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
		require.Equal(t, "ai-gateway", serviceName(t))
	})
	t.Run("OTEL_SERVICE_NAME overrides the default", func(t *testing.T) {
		t.Setenv("OTEL_SERVICE_NAME", "ai-gateway-staging")
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
		require.Equal(t, "ai-gateway-staging", serviceName(t))
	})
}

func newTracedRouter(handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.POST("/v1/messages", handler)
	r.GET("/health", handler)
	r.NoRoute(handler)
	return r
}

func TestMiddlewareContinuesCallerTraceAndEnrichesLogger(t *testing.T) {
	rec := installRecorder(t)

	core, logs := observer.New(zap.InfoLevel)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Stand-in for middleware.RequestLogger, which must run before tracing.
	r.Use(func(c *gin.Context) {
		ctx := logger.IntoContext(c.Request.Context(), zap.New(core).With(zap.String("request_id", "rid-1")))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.Use(Middleware())
	r.POST("/v1/messages", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), ctxkey.AccountID, int64(42))
		ctx = context.WithValue(ctx, ctxkey.UserID, int64(7))
		ctx = context.WithValue(ctx, ctxkey.Platform, "anthropic")
		ctx = context.WithValue(ctx, ctxkey.Model, "claude-sonnet-5-5")
		c.Request = c.Request.WithContext(ctx)
		logger.FromContext(c.Request.Context()).Info("handled")
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", nil)
	req.Header.Set("traceparent", "00-"+callerTraceID+"-"+callerSpanID+"-01")
	r.ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	require.Equal(t, "POST /v1/messages", span.Name())
	require.Equal(t, trace.SpanKindServer, span.SpanKind())
	require.Equal(t, callerTraceID, span.SpanContext().TraceID().String())
	require.Equal(t, callerSpanID, span.Parent().SpanID().String())
	require.True(t, span.Parent().IsRemote())

	attrs := span.Attributes()
	for key, want := range map[attribute.Key]attribute.Value{
		"http.route":                attribute.StringValue("/v1/messages"),
		"http.response.status_code": attribute.IntValue(http.StatusOK),
		attrAccountID:               attribute.Int64Value(42),
		attrUserID:                  attribute.Int64Value(7),
		attrPlatform:                attribute.StringValue("anthropic"),
		attrModel:                   attribute.StringValue("claude-sonnet-5-5"),
	} {
		got, ok := attrValue(attrs, key)
		require.True(t, ok, "missing attribute %s", key)
		require.Equal(t, want, got, "attribute %s", key)
	}
	require.Equal(t, codes.Unset, span.Status().Code)

	entries := logs.All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, callerTraceID, fields["trace_id"])
	require.Equal(t, span.SpanContext().SpanID().String(), fields["span_id"])
	require.Equal(t, "rid-1", fields["request_id"])
}

func TestMiddlewareStartsNewTraceWithoutTraceparent(t *testing.T) {
	rec := installRecorder(t)
	r := newTracedRouter(func(c *gin.Context) { c.Status(http.StatusBadGateway) })

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	spans := rec.Ended()
	require.Len(t, spans, 1)
	require.True(t, spans[0].SpanContext().TraceID().IsValid())
	require.False(t, spans[0].Parent().IsValid())
	require.Equal(t, codes.Error, spans[0].Status().Code)
}

func TestMiddlewareSkipsProbesAndUnroutedRequests(t *testing.T) {
	rec := installRecorder(t)
	called := 0
	r := newTracedRouter(func(c *gin.Context) { called++; c.Status(http.StatusOK) })

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))

	require.Equal(t, 2, called)
	require.Empty(t, rec.Ended())
}

func TestMiddlewareDisabledPassesThrough(t *testing.T) {
	active.Store(nil)
	var sawSpan bool
	r := newTracedRouter(func(c *gin.Context) {
		sawSpan = trace.SpanContextFromContext(c.Request.Context()).IsValid()
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("traceparent", "00-"+callerTraceID+"-"+callerSpanID+"-01")
	r.ServeHTTP(httptest.NewRecorder(), req)

	require.False(t, sawSpan)
}

// parentContext returns a context carrying a recorded server span, as the
// middleware would provide to handlers.
func parentContext(t *testing.T) (context.Context, trace.Span) {
	t.Helper()
	state := active.Load()
	require.NotNil(t, state)
	return state.tracer.Start(context.Background(), "POST /v1/messages", trace.WithSpanKind(trace.SpanKindServer))
}

func TestUpstreamSpanCoversStreamedBodyWithoutInjectingHeaders(t *testing.T) {
	rec := installRecorder(t)

	var upstreamHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: hello\n\n")
	}))
	defer upstream.Close()

	ctx, parent := parentContext(t)
	defer parent.End()

	client := InstrumentUpstreamClient(upstream.Client(), 99)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/v1beta/models/x:streamGenerateContent?key=SECRET&alt=sse", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)

	require.Empty(t, upstreamHeaders.Get("traceparent"))
	require.Empty(t, upstreamHeaders.Get("tracestate"))
	require.Empty(t, upstreamHeaders.Get("baggage"))
	require.Empty(t, rec.Ended(), "span must stay open until the streamed body is consumed")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "data: hello\n\n", string(body))
	require.NoError(t, resp.Body.Close())

	spans := rec.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	require.Equal(t, trace.SpanKindClient, span.SpanKind())
	require.Equal(t, parent.SpanContext().TraceID(), span.SpanContext().TraceID())
	require.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID())

	fullURL, ok := attrValue(span.Attributes(), "url.full")
	require.True(t, ok)
	require.NotContains(t, fullURL.AsString(), "SECRET")
	require.True(t, strings.HasSuffix(fullURL.AsString(), "/v1beta/models/x:streamGenerateContent"))
	account, ok := attrValue(span.Attributes(), attrAccountID)
	require.True(t, ok)
	require.Equal(t, int64(99), account.AsInt64())
	status, ok := attrValue(span.Attributes(), "http.response.status_code")
	require.True(t, ok)
	require.Equal(t, int64(http.StatusOK), status.AsInt64())

	var sawFirstByte bool
	for _, ev := range span.Events() {
		sawFirstByte = sawFirstByte || ev.Name == "first_byte"
	}
	require.True(t, sawFirstByte)
}

func TestUpstreamSpanSkipsRequestsOutsideATrace(t *testing.T) {
	rec := installRecorder(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	client := InstrumentUpstreamClient(upstream.Client(), 1)
	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Empty(t, rec.Ended())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestUpstreamSpanErrorDoesNotLeakQuery(t *testing.T) {
	rec := installRecorder(t)
	client := InstrumentUpstreamClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: errors.New("connection reset")}
	})}, 1)

	ctx, parent := parentContext(t)
	defer parent.End()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models?key=SECRET", nil)
	require.NoError(t, err)
	_, err = client.Do(req) //nolint:bodyclose // the request fails, there is no body
	require.Error(t, err)

	spans := rec.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Error, spans[0].Status().Code)
	require.Equal(t, "connection reset", spans[0].Status().Description)
	for _, kv := range spans[0].Attributes() {
		require.NotContains(t, kv.Value.String(), "SECRET")
	}
}

func TestUpstreamSpanMarksHTTPErrors(t *testing.T) {
	rec := installRecorder(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	ctx, parent := parentContext(t)
	defer parent.End()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, nil)
	require.NoError(t, err)
	resp, err := InstrumentUpstreamClient(upstream.Client(), 1).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	spans := rec.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Error, spans[0].Status().Code)
}

// cancelWaitBody delivers one chunk, then blocks like a network read until
// the request context is done, returning its error as net/http does.
type cancelWaitBody struct {
	ctx     context.Context
	reading chan struct{}
	served  bool
}

func (b *cancelWaitBody) Read(p []byte) (int, error) {
	if !b.served {
		b.served = true
		return copy(p, "data: done\n\n"), nil
	}
	close(b.reading)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancelWaitBody) Close() error { return nil }

// The gateway cancels an attempt itself: it closes the body right after a
// terminal SSE event (cancelling the request first), or the client leaves. A
// read still in flight at that moment, such as the zstd decoder's read-ahead,
// fails with context.Canceled. That is not an upstream error.
func TestUpstreamSpanLocalCancellationIsNotAnError(t *testing.T) {
	rec := installRecorder(t)
	var body *cancelWaitBody
	client := InstrumentUpstreamClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body = &cancelWaitBody{ctx: req.Context(), reading: make(chan struct{})}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body, Request: req}, nil
	})}, 7)

	parentCtx, parent := parentContext(t)
	defer parent.End()
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)

	buf := make([]byte, 64)
	_, err = resp.Body.Read(buf)
	require.NoError(t, err)

	readErr := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(buf)
		readErr <- err
	}()
	<-body.reading
	cancel()
	require.ErrorIs(t, <-readErr, context.Canceled)
	require.NoError(t, resp.Body.Close())

	spans := rec.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Unset, spans[0].Status().Code)
}

func TestUpstreamSpanCancelledBeforeResponseIsNotAnError(t *testing.T) {
	rec := installRecorder(t)
	client := InstrumentUpstreamClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}, 7)

	parentCtx, parent := parentContext(t)
	defer parent.End()
	ctx, cancel := context.WithCancel(parentCtx)
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	require.NoError(t, err)
	_, err = client.Do(req) //nolint:bodyclose // the request fails, there is no body
	require.ErrorIs(t, err, context.Canceled)

	spans := rec.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Unset, spans[0].Status().Code)
}

// A deadline is a timeout, not a cancellation: the upstream was too slow.
func TestUpstreamSpanDeadlineIsAnError(t *testing.T) {
	rec := installRecorder(t)
	client := InstrumentUpstreamClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}, 7)

	parentCtx, parent := parentContext(t)
	defer parent.End()
	ctx, cancel := context.WithDeadline(parentCtx, time.Now())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	require.NoError(t, err)
	_, err = client.Do(req) //nolint:bodyclose // the request fails, there is no body
	require.ErrorIs(t, err, context.DeadlineExceeded)

	spans := rec.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Error, spans[0].Status().Code)
}

func TestInstrumentUpstreamClientDisabledReturnsSameClient(t *testing.T) {
	active.Store(nil)
	client := &http.Client{}
	require.Same(t, client, InstrumentUpstreamClient(client, 1))
}
