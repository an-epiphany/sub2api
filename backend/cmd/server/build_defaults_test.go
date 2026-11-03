package main

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func loadLogFormat(t *testing.T) string {
	t.Helper()
	viper.Reset()
	cfg, err := config.Load()
	require.NoError(t, err)
	return cfg.Log.Format
}

func TestApplyBuildDefaultsLogFormat(t *testing.T) {
	t.Cleanup(viper.Reset)
	t.Cleanup(func() {
		config.SetDefaultLogFormat("console")
		logger.SetBootstrapFormat("console")
	})
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATA_DIR", "")
	t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("LOG_FORMAT", "")

	applyBuildDefaults("source")
	require.Equal(t, "console", loadLogFormat(t), "source builds (local development) keep console logs")

	applyBuildDefaults(buildTypeRelease)
	require.Equal(t, "json", loadLogFormat(t), "packaged builds default to JSON logs")

	t.Setenv("LOG_FORMAT", "console")
	require.Equal(t, "console", loadLogFormat(t), "explicit LOG_FORMAT wins over the build default")
}
