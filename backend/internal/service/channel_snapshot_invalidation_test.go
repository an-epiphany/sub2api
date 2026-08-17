//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 渠道快照持有定价、模型映射和模型白名单，直接决定计费单价与调度准入。
// 管理请求只会落到一个副本，其余副本必须靠广播失效，否则最长 channelCacheTTL
// （10 分钟）内同一个用户在不同副本会被按不同单价扣费。
func TestChannelService_InvalidateCacheReachesPeerReplica(t *testing.T) {
	shared := newFakeSnapshotInvalidationCache()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var peerReloads atomic.Int64
	peerRepo := &mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) {
		peerReloads.Add(1)
		return nil, nil
	}}
	peer := NewChannelService(peerRepo, &stubGroupRepoForAvailable{}, nil, nil)
	peer.SetSnapshotInvalidationCache(ctx, shared)

	writerRepo := &mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) {
		return nil, nil
	}}
	writer := NewChannelService(writerRepo, &stubGroupRepoForAvailable{}, nil, nil)
	writer.SetSnapshotInvalidationCache(ctx, shared)

	require.Eventually(t, func() bool {
		return shared.subscriberCount(SnapshotTopicChannel) == 2
	}, time.Second, 5*time.Millisecond, "两个副本都应订阅渠道失效广播")

	before := peerReloads.Load()
	writer.invalidateCache()

	require.Greater(t, peerReloads.Load(), before,
		"处理管理请求的副本失效后，同伴副本必须也重建渠道快照")
}

// 未接入广播通道时（单实例部署 / 单元测试）行为不变：只失效并重建本地快照。
func TestChannelService_InvalidateCacheWithoutBroadcastStaysLocal(t *testing.T) {
	var reloads atomic.Int64
	repo := &mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) {
		reloads.Add(1)
		return nil, nil
	}}
	svc := NewChannelService(repo, &stubGroupRepoForAvailable{}, nil, nil)

	before := reloads.Load()
	svc.invalidateCache()

	require.Greater(t, reloads.Load(), before, "无广播通道时仍必须重建本地快照")
}
