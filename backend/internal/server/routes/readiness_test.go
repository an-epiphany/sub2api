//go:build unit

package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// K8s 滚动更新时，Pod 从 Service endpoints 摘除是异步的：SIGTERM 之后还会有一小段
// 时间继续收到新请求。进程必须能对外声明「我正在关闭」，否则运维只能靠 preStop sleep
// 硬等。/health 同时被 livenessProbe 和 Dockerfile HEALTHCHECK 使用，关闭期返回非 200
// 会被误杀，所以关闭状态必须由独立的 /readyz 承载。

func newReadinessRouter() (*gin.Engine, *DrainState) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	drain := NewDrainState()
	RegisterCommonRoutes(r, drain)
	return r, drain
}

func get(t *testing.T, r *gin.Engine, path string) int {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	r.ServeHTTP(w, req)
	return w.Code
}

func TestReadyz_ReturnsOKWhileServing(t *testing.T) {
	r, _ := newReadinessRouter()
	require.Equal(t, http.StatusOK, get(t, r, "/readyz"))
}

func TestReadyz_ReturnsServiceUnavailableWhileDraining(t *testing.T) {
	r, drain := newReadinessRouter()
	drain.BeginDraining()
	require.Equal(t, http.StatusServiceUnavailable, get(t, r, "/readyz"),
		"关闭期 readiness 必须失败，K8s 才会把本 Pod 摘出 endpoints")
}

// /health 是 liveness：关闭期仍然要返回 200，否则 kubelet 会在优雅退出中途把进程杀掉，
// 在途的流式响应反而被截断得更早。
func TestHealth_StaysOKWhileDraining(t *testing.T) {
	r, drain := newReadinessRouter()
	drain.BeginDraining()
	require.Equal(t, http.StatusOK, get(t, r, "/health"),
		"liveness 不能随关闭状态翻转，否则会被 kubelet 提前杀死")
}

func TestDrainState_BeginDrainingIsIdempotent(t *testing.T) {
	drain := NewDrainState()
	require.False(t, drain.IsDraining())
	drain.BeginDraining()
	drain.BeginDraining()
	require.True(t, drain.IsDraining())
}
