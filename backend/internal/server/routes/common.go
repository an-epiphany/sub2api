package routes

import (
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// DrainState 表示进程是否已经进入优雅关闭。
//
// K8s 把 Pod 从 Service endpoints 摘除是异步的：收到 SIGTERM 之后进程还会继续
// 收到一小段时间的新请求。有了这个状态位，readinessProbe 可以立刻失败，让
// endpoint 尽快摘除，而不必让运维靠 preStop sleep 去硬等一个猜出来的时长。
type DrainState struct {
	draining atomic.Bool
}

func NewDrainState() *DrainState { return &DrainState{} }

// BeginDraining 标记进程进入关闭流程，可重复调用。
func (d *DrainState) BeginDraining() {
	if d == nil {
		return
	}
	d.draining.Store(true)
}

func (d *DrainState) IsDraining() bool {
	return d != nil && d.draining.Load()
}

// RegisterCommonRoutes 注册通用路由（健康检查、就绪探针、状态等）。
func RegisterCommonRoutes(r *gin.Engine, drain *DrainState) {
	// 存活探针（livenessProbe / Dockerfile HEALTHCHECK）。
	// 关闭期刻意保持 200：liveness 失败会让 kubelet 直接杀进程，
	// 在途的流式响应反而被截断得更早。
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 就绪探针（readinessProbe）。收到 SIGTERM 后立即失败，
	// 让 K8s 把本 Pod 摘出 Service endpoints，新流量不再进来。
	//
	// 刻意不探 DB/Redis：所有副本共享同一套依赖，探依赖会让整个 Deployment
	// 同时 NotReady、endpoints 清零，把局部故障放大成全站故障。
	// 依赖不可用时进程根本起不来（迁移与密钥引导在监听端口之前同步完成），
	// 不存在「已 Ready 但未初始化」的窗口。
	r.GET("/readyz", func(c *gin.Context) {
		if drain.IsDraining() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "draining"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Claude Code 遥测日志（忽略，直接返回200）
	r.POST("/api/event_logging/batch", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Setup status endpoint (always returns needs_setup: false in normal mode)
	// This is used by the frontend to detect when the service has restarted after setup
	r.GET("/setup/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"needs_setup": false,
				"step":        "completed",
			},
		})
	})
}
