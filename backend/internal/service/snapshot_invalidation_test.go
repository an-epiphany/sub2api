//go:build unit

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 渠道定价、模型映射、模型白名单这些进程内快照，在单实例下由 CRUD 直接失效即可；
// 多副本下「哪个副本处理了这次管理请求」是随机的，其余副本必须收到广播才会失效，
// 否则同一个用户连续两个请求打到不同副本会被按不同单价计费。

// fakeSnapshotInvalidationCache 用一个进程内的订阅者列表模拟 Redis Pub/Sub 的扇出。
type fakeSnapshotInvalidationCache struct {
	mu        sync.Mutex
	handlers  map[string][]func()
	published map[string]int
	pubErr    error
}

func newFakeSnapshotInvalidationCache() *fakeSnapshotInvalidationCache {
	return &fakeSnapshotInvalidationCache{
		handlers:  map[string][]func(){},
		published: map[string]int{},
	}
}

func (f *fakeSnapshotInvalidationCache) PublishSnapshotInvalidation(_ context.Context, topic string) error {
	if f.pubErr != nil {
		return f.pubErr
	}
	f.mu.Lock()
	f.published[topic]++
	handlers := append([]func(){}, f.handlers[topic]...)
	f.mu.Unlock()
	for _, h := range handlers {
		h()
	}
	return nil
}

func (f *fakeSnapshotInvalidationCache) SubscribeSnapshotInvalidation(ctx context.Context, topic string, handler func()) error {
	f.mu.Lock()
	f.handlers[topic] = append(f.handlers[topic], handler)
	f.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeSnapshotInvalidationCache) publishCount(topic string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published[topic]
}

func (f *fakeSnapshotInvalidationCache) subscriberCount(topic string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.handlers[topic])
}

// 广播器把「本地失效」和「通知同伴」拆开，订阅端只做本地失效，
// 否则收到广播后又发一次广播，会在副本之间无限回弹。
func TestSnapshotInvalidator_BroadcastReachesPeersWithoutEchoing(t *testing.T) {
	cache := newFakeSnapshotInvalidationCache()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var peerInvalidations atomic.Int64
	peer := NewSnapshotInvalidator(cache, "channel", func() { peerInvalidations.Add(1) })
	peer.Start(ctx)

	var selfInvalidations atomic.Int64
	self := NewSnapshotInvalidator(cache, "channel", func() { selfInvalidations.Add(1) })
	self.Start(ctx)

	require.Eventually(t, func() bool { return cache.subscriberCount("channel") == 2 },
		time.Second, 5*time.Millisecond)

	self.InvalidateAndBroadcast(ctx)

	require.EqualValues(t, 1, cache.publishCount("channel"), "一次失效只能广播一次")
	require.EqualValues(t, 1, peerInvalidations.Load(), "同伴副本必须收到失效通知")
	require.EqualValues(t, 1, selfInvalidations.Load(), "本副本也要失效（订阅端统一处理，不必额外调用）")
}

// Redis 广播失败时必须仍然完成本地失效：宁可只有本副本生效，也不能连本地都不刷新。
func TestSnapshotInvalidator_LocalInvalidationSurvivesBroadcastFailure(t *testing.T) {
	cache := newFakeSnapshotInvalidationCache()
	cache.pubErr = context.DeadlineExceeded
	ctx := context.Background()

	var invalidations atomic.Int64
	inv := NewSnapshotInvalidator(cache, "channel", func() { invalidations.Add(1) })

	inv.InvalidateAndBroadcast(ctx)

	require.EqualValues(t, 1, invalidations.Load(), "广播失败也必须完成本地失效")
}

// 未接入共享缓存时（单实例部署、单元测试）退化为纯本地失效，不能 panic。
func TestSnapshotInvalidator_NilCacheFallsBackToLocalOnly(t *testing.T) {
	var invalidations atomic.Int64
	inv := NewSnapshotInvalidator(nil, "channel", func() { invalidations.Add(1) })
	inv.Start(context.Background())

	inv.InvalidateAndBroadcast(context.Background())

	require.EqualValues(t, 1, invalidations.Load())
}
