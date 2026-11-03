package main

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const buildTypeRelease = "release"

func init() {
	applyBuildDefaults(BuildType)
}

// applyBuildDefaults makes packaged builds (CI release binaries and Docker
// images, which set BuildType=release via ldflags) log JSON by default, so log
// collectors can parse them. Source builds used for local development keep
// the human-readable console format. An explicit log.format or LOG_FORMAT
// always wins over this default.
func applyBuildDefaults(buildType string) {
	if buildType != buildTypeRelease {
		return
	}
	config.SetDefaultLogFormat("json")
	logger.SetBootstrapFormat("json")
}
