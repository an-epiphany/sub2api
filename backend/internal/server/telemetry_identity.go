package server

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/telemetry"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
)

// telemetryRequestAttributes records on the request's server span which API
// key the request was made with. The key is already in memory: the API key
// auth middleware puts it on the gin context.
func telemetryRequestAttributes(c *gin.Context) []attribute.KeyValue {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		// Recorded when the key exists but the request was rejected (quota,
		// IP rules, ...), which is when knowing the key helps most.
		apiKey, ok = middleware2.GetOpsFallbackAPIKey(c)
	}
	if !ok || apiKey == nil {
		return nil
	}
	attrs := []attribute.KeyValue{
		telemetry.AttrAPIKeyID.Int64(apiKey.ID),
		telemetry.AttrAPIKeyMasked.String(maskAPIKey(apiKey.Key)),
	}
	if apiKey.Name != "" {
		attrs = append(attrs, telemetry.AttrAPIKeyName.String(apiKey.Name))
	}
	return attrs
}

// maskAPIKey keeps only the last four characters, as sub2api does elsewhere.
func maskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "****"
	}
	return strings.Repeat("*", 8) + key[len(key)-4:]
}
