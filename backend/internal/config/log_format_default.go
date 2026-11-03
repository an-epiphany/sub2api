package config

// defaultLogFormat is the log.format default used when neither the config
// file nor LOG_FORMAT sets it.
var defaultLogFormat = "console"

// SetDefaultLogFormat changes the log.format default. Call it before the
// config is loaded; explicit configuration still takes precedence.
func SetDefaultLogFormat(format string) {
	defaultLogFormat = format
}
