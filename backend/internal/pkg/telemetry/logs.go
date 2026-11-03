package telemetry

import (
	"context"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func startLogs(ctx context.Context, cfg exporterConfig, res *resource.Resource) (func(context.Context) error, error) {
	exporter, err := newLogExporter(ctx, cfg.protocol)
	if err != nil {
		return nil, fmt.Errorf("create otlp log exporter: %w", err)
	}
	// The SDK reads OTEL_BLRP_* for the batch processor itself.
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(res),
	)
	detach, err := attachLogExport(provider)
	if err != nil {
		_ = provider.Shutdown(ctx)
		return nil, fmt.Errorf("attach otlp log export: %w", err)
	}
	return func(ctx context.Context) error {
		// Detach first so nothing is emitted into a provider being shut down.
		return errors.Join(detach(), provider.Shutdown(ctx))
	}, nil
}

func newLogExporter(ctx context.Context, protocol string) (sdklog.Exporter, error) {
	// Both exporters read endpoint, headers, TLS, compression and timeout
	// from the standard OTEL_EXPORTER_OTLP_* variables.
	if protocol == protocolGRPC {
		return otlploggrpc.New(ctx)
	}
	return otlploghttp.New(ctx)
}

// attachLogExport adds an otelzap core to the global zap logger, so every log
// entry that reaches stdout/file is also exported, at the same live level.
func attachLogExport(provider log.LoggerProvider) (func() error, error) {
	return logger.AddCore(func(level zapcore.LevelEnabler) zapcore.Core {
		core, err := zapcore.NewIncreaseLevelCore(otelzap.NewCore(instrumentationName, otelzap.WithLoggerProvider(provider)), level)
		if err != nil {
			otel.Handle(fmt.Errorf("level-filter otlp log core: %w", err))
			return nil
		}
		return skipSelfCore{Core: core}
	})
}

// skipSelfCore drops the SDK's own error reports (logged under
// selfLoggerName). Exporting them would loop: a failing exporter reports an
// error, which is logged, exported through the same exporter, fails again.
// They still reach stdout/file through the other cores.
type skipSelfCore struct {
	zapcore.Core
}

func (c skipSelfCore) With(fields []zapcore.Field) zapcore.Core {
	return skipSelfCore{Core: c.Core.With(fields)}
}

func (c skipSelfCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if entry.LoggerName == selfLoggerName {
		return ce
	}
	return c.Core.Check(entry, ce)
}

// contextField carries ctx to the otelzap core, which uses a context.Context
// field value as the log record context (trace and span IDs). zap's encoders
// skip SkipType fields, so it does not appear in stdout/file output.
func contextField(ctx context.Context) zap.Field {
	return zap.Field{Key: "otel_context", Type: zapcore.SkipType, Interface: ctx}
}
