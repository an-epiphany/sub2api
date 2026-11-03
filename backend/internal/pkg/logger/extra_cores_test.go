package logger

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestAddCoreReceivesEntriesAtLiveLevelAndSurvivesReconfigure(t *testing.T) {
	require.NoError(t, Init(InitOptions{Level: "info", Format: "json", Output: OutputOptions{ToStdout: true}}))

	var observed *observer.ObservedLogs
	remove, err := AddCore(func(level zapcore.LevelEnabler) zapcore.Core {
		core, logs := observer.New(level)
		observed = logs
		return core
	})
	require.NoError(t, err)

	L().Debug("hidden")
	L().Info("visible")
	require.Equal(t, 1, observed.Len())
	require.Equal(t, "visible", observed.All()[0].Message)

	// Runtime level changes apply to the extra core too.
	require.NoError(t, SetLevel("debug"))
	L().Debug("now visible")
	require.Equal(t, 2, observed.Len())

	// Reconfigure rebuilds the logger; the extra core stays attached.
	require.NoError(t, Reconfigure(func(o *InitOptions) error { o.Level = "info"; return nil }))
	L().Info("after reconfigure")
	require.Equal(t, 1, observed.FilterMessage("after reconfigure").Len())

	require.NoError(t, remove())
	L().Info("after remove")
	require.Equal(t, 0, observed.FilterMessage("after remove").Len())
}
