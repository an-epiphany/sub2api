package telemetry

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Attribute keys for gateway routing results, recorded on the server span.
const (
	attrAccountID = attribute.Key("sub2api.account_id")
	attrUserID    = attribute.Key("sub2api.user_id")
	attrPlatform  = attribute.Key("sub2api.platform")
	attrModel     = attribute.Key("gen_ai.request.model")
)

// Attribute keys for the API key a request was made with, for
// RequestAttributes.
const (
	AttrAPIKeyID     = attribute.Key("sub2api.api_key.id")
	AttrAPIKeyName   = attribute.Key("sub2api.api_key.name")
	AttrAPIKeyMasked = attribute.Key("sub2api.api_key.masked")
)

// RequestAttributes returns extra attributes for a request's server span. It
// runs after the handlers, so it sees what they and the auth middleware
// recorded on the gin context, and only for requests that are traced.
type RequestAttributes func(c *gin.Context) []attribute.KeyValue

// Middleware starts a server span for each routed request. The span continues
// the caller's trace when the request carries a W3C traceparent header. The
// trace context is added to the request-scoped logger, so it must run after
// middleware.RequestLogger, which creates that logger.
//
// Requests without a matched route (embedded frontend assets, 404s) and health
// probes are not traced.
func Middleware(extra ...RequestAttributes) gin.HandlerFunc {
	return func(c *gin.Context) {
		state := active.Load()
		route := c.FullPath()
		if state == nil || c.Request == nil || route == "" || isProbeRoute(route) {
			c.Next()
			return
		}

		req := c.Request
		ctx := state.propagator.Extract(req.Context(), propagation.HeaderCarrier(req.Header))
		ctx, span := state.tracer.Start(ctx, req.Method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(req.Method),
				semconv.HTTPRoute(route),
				semconv.URLPath(req.URL.Path),
				semconv.ClientAddress(ip.GetClientIP(c)),
				semconv.UserAgentOriginal(req.UserAgent()),
			),
		)
		defer span.End()

		if sc := span.SpanContext(); sc.IsValid() {
			// trace_id/span_id are for stdout/file readers; contextField lets
			// the OTLP log export attach the same trace context natively.
			requestLogger := logger.FromContext(ctx).With(
				zap.String("trace_id", sc.TraceID().String()),
				zap.String("span_id", sc.SpanID().String()),
				contextField(ctx),
			)
			ctx = logger.IntoContext(ctx, requestLogger)
		}
		c.Request = req.WithContext(ctx)

		c.Next()

		status := c.Writer.Status()
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		// Handlers record the routing result on the request context; read it
		// from c.Request, which downstream middleware may have replaced.
		span.SetAttributes(gatewayAttributes(c.Request)...)
		for _, attrs := range extra {
			span.SetAttributes(attrs(c)...)
		}
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	}
}

func isProbeRoute(route string) bool {
	return route == "/health" || route == "/setup/status"
}

func gatewayAttributes(req *http.Request) []attribute.KeyValue {
	if req == nil {
		return nil
	}
	ctx := req.Context()
	attrs := make([]attribute.KeyValue, 0, 4)
	if v, ok := ctx.Value(ctxkey.AccountID).(int64); ok && v > 0 {
		attrs = append(attrs, attrAccountID.Int64(v))
	}
	if v, ok := ctx.Value(ctxkey.UserID).(int64); ok && v > 0 {
		attrs = append(attrs, attrUserID.Int64(v))
	}
	if v, ok := ctx.Value(ctxkey.Platform).(string); ok && v != "" {
		attrs = append(attrs, attrPlatform.String(v))
	}
	if v, ok := ctx.Value(ctxkey.Model).(string); ok && v != "" {
		attrs = append(attrs, attrModel.String(v))
	}
	return attrs
}
