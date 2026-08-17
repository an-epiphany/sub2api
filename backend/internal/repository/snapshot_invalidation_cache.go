package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// snapshotInvalidationChannelPrefix 是进程内快照失效广播的 Pub/Sub 频道前缀。
const snapshotInvalidationChannelPrefix = "snapshot:invalidate:"

type snapshotInvalidationCache struct {
	rdb *redis.Client
}

// NewSnapshotInvalidationCache 返回 Redis 支撑的快照失效广播通道。
// rdb 为 nil 时返回 nil，调用方会退化为纯本地失效（单实例部署）。
func NewSnapshotInvalidationCache(rdb *redis.Client) service.SnapshotInvalidationCache {
	if rdb == nil {
		return nil
	}
	return &snapshotInvalidationCache{rdb: rdb}
}

func (c *snapshotInvalidationCache) channel(topic string) string {
	return snapshotInvalidationChannelPrefix + topic
}

func (c *snapshotInvalidationCache) PublishSnapshotInvalidation(ctx context.Context, topic string) error {
	return c.rdb.Publish(ctx, c.channel(topic), "invalidate").Err()
}

func (c *snapshotInvalidationCache) SubscribeSnapshotInvalidation(ctx context.Context, topic string, handler func()) error {
	pubsub := c.rdb.Subscribe(ctx, c.channel(topic))
	defer func() { _ = pubsub.Close() }()

	// 等待订阅真正建立，避免连接失败时静默地「订阅成功但收不到消息」。
	if _, err := pubsub.Receive(ctx); err != nil {
		return fmt.Errorf("subscribe snapshot invalidation %s: %w", topic, err)
	}

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok || msg == nil {
				return fmt.Errorf("snapshot invalidation channel %s closed", topic)
			}
			handler()
		}
	}
}
