//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// heartbeatCacheStub 同时实现 ConcurrencyCache 与 InstanceRegistryCache。
type heartbeatCacheStub struct {
	stubConcurrencyCacheForTest
	interval time.Duration

	mu       sync.Mutex
	reported []string
}

func (c *heartbeatCacheStub) HeartbeatInstance(_ context.Context, requestPrefix string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reported = append(c.reported, requestPrefix)
	return nil
}

func (c *heartbeatCacheStub) InstanceHeartbeatInterval() time.Duration { return c.interval }

func (c *heartbeatCacheStub) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.reported...)
}

// 心跳必须周期性重复上报。一旦停止，同伴实例的启动清扫就会把本进程的在途槽位
// 当作死进程残留回收，账号/用户并发上限被瞬间架空——正是这个心跳在阻止那件事。
func TestStartInstanceHeartbeat_KeepsReportingSelf(t *testing.T) {
	cache := &heartbeatCacheStub{interval: 5 * time.Millisecond}
	svc := NewConcurrencyService(cache)

	stop := svc.StartInstanceHeartbeat()
	t.Cleanup(stop)

	require.Eventually(t, func() bool {
		return len(cache.snapshot()) >= 3
	}, 2*time.Second, 5*time.Millisecond, "心跳应按间隔重复上报")

	for _, prefix := range cache.snapshot() {
		require.Equal(t, RequestIDPrefix(), prefix, "上报的必须是本进程的 requestID 前缀")
	}
}

// stop 之后不能再有新的心跳，否则进程退出后仍会把自己续期成「存活」。
func TestStartInstanceHeartbeat_StopHaltsReporting(t *testing.T) {
	cache := &heartbeatCacheStub{interval: 5 * time.Millisecond}
	svc := NewConcurrencyService(cache)

	stop := svc.StartInstanceHeartbeat()
	require.Eventually(t, func() bool {
		return len(cache.snapshot()) >= 1
	}, 2*time.Second, 5*time.Millisecond)

	stop()
	time.Sleep(20 * time.Millisecond)
	settled := len(cache.snapshot())
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, settled, len(cache.snapshot()), "stop 之后不应再有心跳")
}

// 缓存不支持实例注册表（例如测试替身或未来的其他实现）时必须安全降级。
func TestStartInstanceHeartbeat_UnsupportedCacheIsNoop(t *testing.T) {
	svc := NewConcurrencyService(&stubConcurrencyCacheForTest{})

	stop := svc.StartInstanceHeartbeat()
	require.NotNil(t, stop)
	stop()
}

// 间隔非正数时不启动心跳，避免 time.NewTicker panic。
func TestStartInstanceHeartbeat_NonPositiveIntervalIsNoop(t *testing.T) {
	cache := &heartbeatCacheStub{interval: 0}
	svc := NewConcurrencyService(cache)

	stop := svc.StartInstanceHeartbeat()
	t.Cleanup(stop)

	time.Sleep(20 * time.Millisecond)
	require.Empty(t, cache.snapshot(), "间隔非正数时不应上报心跳")
}
