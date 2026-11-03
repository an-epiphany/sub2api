package main

import (
	"context"
	"log"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/telemetry"
)

const telemetryShutdownTimeout = 5 * time.Second

// initTelemetry enables OpenTelemetry trace and log export when the OTEL_*
// environment asks for it. Telemetry is observability only: a
// misconfiguration is logged and the server keeps running without it rather
// than refusing to start.
func initTelemetry(cfg *config.Config) func(context.Context) error {
	shutdown, err := telemetry.Init(context.Background(), telemetry.Options{
		ServiceVersion: Version,
		Environment:    cfg.Log.Environment,
	})
	if err != nil {
		log.Printf("OpenTelemetry disabled: %v", err)
	}
	return shutdown
}

// shutdownTelemetry flushes buffered spans and logs before the process exits.
func shutdownTelemetry(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		log.Printf("OpenTelemetry shutdown: %v", err)
	}
}
