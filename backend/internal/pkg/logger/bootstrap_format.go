package logger

// bootstrapFormat is the output format of the bootstrap logger, which runs
// before the config (and its log.format) is loaded.
var bootstrapFormat = "console"

// SetBootstrapFormat changes the bootstrap logger's format. Call it before
// InitBootstrap.
func SetBootstrapFormat(format string) {
	bootstrapFormat = format
}
