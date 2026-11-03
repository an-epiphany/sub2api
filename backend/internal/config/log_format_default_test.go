package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func withDefaultLogFormat(t *testing.T, format string) {
	t.Helper()
	prev := defaultLogFormat
	SetDefaultLogFormat(format)
	t.Cleanup(func() { SetDefaultLogFormat(prev) })
}

func TestLoadLogFormatDefaultsToConsole(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("LOG_FORMAT", "")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "console", cfg.Log.Format)
}

func TestLoadLogFormatUsesOverriddenDefault(t *testing.T) {
	resetViperWithJWTSecret(t)
	withDefaultLogFormat(t, "json")
	t.Setenv("LOG_FORMAT", "")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "json", cfg.Log.Format)
}

func TestLoadLogFormatExplicitEnvWinsOverDefault(t *testing.T) {
	resetViperWithJWTSecret(t)
	withDefaultLogFormat(t, "json")
	t.Setenv("LOG_FORMAT", "console")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "console", cfg.Log.Format)
}
