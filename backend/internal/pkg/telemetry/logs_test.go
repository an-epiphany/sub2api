package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// memoryLogExporter keeps exported records in memory.
type memoryLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *memoryLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return nil
}

func (e *memoryLogExporter) Shutdown(context.Context) error   { return nil }
func (e *memoryLogExporter) ForceFlush(context.Context) error { return nil }

func (e *memoryLogExporter) byBody(body string) []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []sdklog.Record
	for _, r := range e.records {
		if r.Body().AsString() == body {
			out = append(out, r)
		}
	}
	return out
}

// installLogRecorder exports the global logger's entries synchronously into
// memory. Tests touching the global logger must not run in parallel.
func installLogRecorder(t *testing.T) *memoryLogExporter {
	t.Helper()
	require.NoError(t, logger.Init(logger.InitOptions{Level: "info", Format: "json", Output: logger.OutputOptions{ToStdout: true}}))
	exp := &memoryLogExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	detach, err := attachLogExport(provider)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = detach()
		_ = provider.Shutdown(context.Background())
	})
	return exp
}

func recordAttrs(r sdklog.Record) map[string]attribute.Value {
	attrs := map[string]attribute.Value{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	return attrs
}

func TestRequestLogsCarryTraceContext(t *testing.T) {
	rec := installRecorder(t)
	exp := installLogRecorder(t)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.POST("/v1/messages", func(c *gin.Context) {
		logger.FromContext(c.Request.Context()).Warn("upstream slow", zap.Int("attempt", 2))
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("traceparent", "00-"+callerTraceID+"-"+callerSpanID+"-01")
	r.ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	require.Len(t, spans, 1)
	records := exp.byBody("upstream slow")
	require.Len(t, records, 1)
	record := records[0]

	require.Equal(t, callerTraceID, record.TraceID().String())
	require.Equal(t, spans[0].SpanContext().SpanID(), record.SpanID())
	require.Equal(t, log.SeverityWarn, record.Severity())

	attrs := recordAttrs(record)
	require.Equal(t, int64(2), attrs["attempt"].AsInt64())
	require.Equal(t, callerTraceID, attrs["trace_id"].AsString())
	require.NotContains(t, attrs, "otel_context")
}

func TestLogExportRespectsLoggerLevel(t *testing.T) {
	exp := installLogRecorder(t)

	logger.L().Debug("debug entry")
	logger.L().Info("info entry")

	require.Empty(t, exp.byBody("debug entry"))
	require.Len(t, exp.byBody("info entry"), 1)
}

func TestSDKSelfErrorsAreNotExported(t *testing.T) {
	exp := installLogRecorder(t)

	logger.L().Named(selfLoggerName).Warn("opentelemetry error")
	logger.L().Warn("regular warning")

	require.Empty(t, exp.byBody("opentelemetry error"))
	require.Len(t, exp.byBody("regular warning"), 1)
}

func TestContextFieldIsInvisibleToZapEncoders(t *testing.T) {
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	l := zap.New(zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.DebugLevel))

	l.With(contextField(context.Background())).Info("hello")

	require.Contains(t, buf.String(), `"msg":"hello"`)
	require.NotContains(t, buf.String(), "otel_context")
}
