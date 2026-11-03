package logger

import (
	"go.uber.org/zap/zapcore"
)

// CoreFactory builds an additional zap core for the global logger. level is
// the logger's live level, so runtime level changes apply to the extra core
// as well.
type CoreFactory func(level zapcore.LevelEnabler) zapcore.Core

type extraCore struct {
	id      uint64
	factory CoreFactory
}

// Guarded by mu.
var (
	extraCores      []extraCore
	nextExtraCoreID uint64
)

// AddCore attaches an extra output to the global logger, next to the
// configured stdout/file outputs, and keeps it across Reconfigure calls. It
// returns a function that detaches the core again.
func AddCore(factory CoreFactory) (remove func() error, err error) {
	mu.Lock()
	defer mu.Unlock()

	nextExtraCoreID++
	id := nextExtraCoreID
	extraCores = append(extraCores, extraCore{id: id, factory: factory})
	if err := initLocked(initOptions); err != nil {
		extraCores = withoutExtraCore(extraCores, id)
		return nil, err
	}

	return func() error {
		mu.Lock()
		defer mu.Unlock()
		extraCores = withoutExtraCore(extraCores, id)
		return initLocked(initOptions)
	}, nil
}

func buildExtraCoresLocked(level zapcore.LevelEnabler) []zapcore.Core {
	cores := make([]zapcore.Core, 0, len(extraCores))
	for _, c := range extraCores {
		if core := c.factory(level); core != nil {
			cores = append(cores, core)
		}
	}
	return cores
}

func withoutExtraCore(cores []extraCore, id uint64) []extraCore {
	out := make([]extraCore, 0, len(cores))
	for _, c := range cores {
		if c.id != id {
			out = append(out, c)
		}
	}
	return out
}
