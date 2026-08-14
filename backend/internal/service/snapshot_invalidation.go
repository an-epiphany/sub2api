package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// SnapshotInvalidationCache 是「进程内快照」的跨实例失效广播通道。
//
// 存在的理由：渠道定价、模型映射、模型白名单、系统设置这些数据都在每个进程里
// 存了一份 atomic.Value 快照。单实例下 CRUD 直接失效本地快照即可；多副本下
// 「哪个副本处理了这次管理请求」是随机的，其余副本只能等 TTL（渠道 10 分钟、
// 设置 60 秒）自然过期。对计费类数据这不是可用性问题而是正确性问题：
// 同一个用户连续两个请求打到不同副本会被按不同单价扣费。
type SnapshotInvalidationCache interface {
	// PublishSnapshotInvalidation 向所有实例（含自己）广播一次失效。
	PublishSnapshotInvalidation(ctx context.Context, topic string) error
	// SubscribeSnapshotInvalidation 订阅失效广播，阻塞直到 ctx 结束或订阅中断。
	SubscribeSnapshotInvalidation(ctx context.Context, topic string, handler func()) error
}

const (
	// SnapshotTopicChannel 覆盖渠道定价 / 模型映射 / 模型白名单快照。
	SnapshotTopicChannel = "channel"
	// SnapshotTopicSettings 覆盖 SettingService 的一组运行时设置快照。
	SnapshotTopicSettings = "settings"

	snapshotBroadcastTimeout    = 3 * time.Second
	snapshotResubscribeMinDelay = time.Second
	snapshotResubscribeMaxDelay = 30 * time.Second
)

// SnapshotInvalidator 把「失效本地快照」和「通知同伴实例」组合起来。
//
// 关键约束：订阅端只执行本地失效，绝不能再广播一次，否则一条失效消息会在副本
// 之间无限回弹。因此本地失效动作（invalidate）由订阅回调统一驱动，
// InvalidateAndBroadcast 只负责发广播 + 在广播不可用时兜底本地失效。
type SnapshotInvalidator struct {
	cache      SnapshotInvalidationCache
	topic      string
	invalidate func()

	startOnce sync.Once
}

func NewSnapshotInvalidator(cache SnapshotInvalidationCache, topic string, invalidate func()) *SnapshotInvalidator {
	return &SnapshotInvalidator{cache: cache, topic: topic, invalidate: invalidate}
}

// Start 启动订阅循环。订阅断开时按指数退避重连——失效广播断了不会立刻出错，
// 但会静默退化回 TTL 一致性，所以必须自愈而不是放弃。
func (s *SnapshotInvalidator) Start(ctx context.Context) {
	if s == nil || s.cache == nil || s.invalidate == nil {
		return
	}
	s.startOnce.Do(func() {
		go func() {
			backoff := snapshotResubscribeMinDelay
			for {
				err := s.cache.SubscribeSnapshotInvalidation(ctx, s.topic, s.invalidate)
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					err = errors.New("snapshot invalidation subscription closed")
				}
				slog.Warn("snapshot invalidation subscriber stopped; retrying",
					"topic", s.topic, "error", err, "retry_in", backoff)
				timer := time.NewTimer(backoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				if backoff < snapshotResubscribeMaxDelay {
					backoff *= 2
					if backoff > snapshotResubscribeMaxDelay {
						backoff = snapshotResubscribeMaxDelay
					}
				}
			}
		}()
	})
}

// InvalidateAndBroadcast 广播失效。广播成功时本地失效由订阅回调完成
// （Redis Pub/Sub 会把消息投递给包括发布者在内的全部订阅者）；
// 广播失败或未配置共享缓存时退化为直接本地失效，保证至少本副本立即生效。
func (s *SnapshotInvalidator) InvalidateAndBroadcast(ctx context.Context) {
	if s == nil || s.invalidate == nil {
		return
	}
	if s.cache == nil {
		s.invalidate()
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	pubCtx, cancel := context.WithTimeout(ctx, snapshotBroadcastTimeout)
	defer cancel()
	if err := s.cache.PublishSnapshotInvalidation(pubCtx, s.topic); err != nil {
		slog.Warn("failed to broadcast snapshot invalidation; falling back to local-only",
			"topic", s.topic, "error", err)
		s.invalidate()
	}
}
