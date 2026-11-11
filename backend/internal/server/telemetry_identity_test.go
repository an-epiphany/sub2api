//go:build unit

package server

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/telemetry"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
)

const testAPIKey = "sk-0123456789abcdef0123456789abcdef0123456789abcdef0123456789a1b2"

func newTelemetryTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	return c
}

func TestTelemetryRequestAttributesRecordsAuthenticatedKey(t *testing.T) {
	c := newTelemetryTestContext()
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 121, Name: "teable-prod", Key: testAPIKey})

	require.ElementsMatch(t, []attribute.KeyValue{
		telemetry.AttrAPIKeyID.Int64(121),
		telemetry.AttrAPIKeyName.String("teable-prod"),
		telemetry.AttrAPIKeyMasked.String("********a1b2"),
	}, telemetryRequestAttributes(c))
}

func TestTelemetryRequestAttributesFallsBackToRejectedKey(t *testing.T) {
	c := newTelemetryTestContext()
	middleware2.SetOpsFallbackAPIKey(c, &service.APIKey{ID: 7, Key: testAPIKey})

	require.ElementsMatch(t, []attribute.KeyValue{
		telemetry.AttrAPIKeyID.Int64(7),
		telemetry.AttrAPIKeyMasked.String("********a1b2"),
	}, telemetryRequestAttributes(c), "an unnamed key records no name")
}

func TestTelemetryRequestAttributesWithoutKey(t *testing.T) {
	require.Empty(t, telemetryRequestAttributes(newTelemetryTestContext()))
}

func TestMaskAPIKey(t *testing.T) {
	for key, want := range map[string]string{
		testAPIKey: "********a1b2",
		"  abcde ": "********bcde",
		"abcd":     "****",
		"":         "",
	} {
		require.Equal(t, want, maskAPIKey(key), "key %q", key)
	}
}
