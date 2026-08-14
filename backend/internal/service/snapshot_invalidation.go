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
	// PublishSnapshotInvalidation 通知其余实例失效各自的快照。
	// 返回 nil 只代表 Redis 收下了这条消息：Pub/Sub 不保证投递，
	// 因此调用方绝不能把它当作「本进程也已失效」。
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
// 之间无限回弹。因此 invalidate 既是写入方的本地失效动作，也是订阅回调本身，
// 而 Pub/Sub 只承担「通知同伴」这一件事。
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

// InvalidateAndBroadcast 通知其余副本，并在返回前同步失效本副本。
//
// 本地失效不能交给「Redis 会把消息投递回发布者」来完成：PUBLISH 返回成功只说明
// Redis 收下了这条消息，投递是异步且不可靠的——本进程的订阅正在启动或断线重连时
// 消息直接丢失，写入方自己的快照会一直陈旧到 TTL（渠道 10 分钟）过期；即便订阅
// 健康，回调也跑在另一条 goroutine 上，写入方紧接着的读依然可能读到旧值。
// 重复失效是幂等的（订阅回调随后可能再失效一次），代价远小于漏失效。
//
// 先广播后本地失效：广播是一次毫秒级的 PUBLISH，而本地失效往往要回读数据库重建
// 快照，把同伴排在它后面只会白白拉长传播延迟。两者都在返回前完成。
//
// 广播使用去掉取消信号的 ctx：数据库写入此时已经落盘，再把请求的取消/超时传播
// 给广播，只会让同伴实例永远收不到这次变更。
func (s *SnapshotInvalidator) InvalidateAndBroadcast(ctx context.Context) {
	if s == nil || s.invalidate == nil {
		return
	}
	if s.cache != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotBroadcastTimeout)
		if err := s.cache.PublishSnapshotInvalidation(pubCtx, s.topic); err != nil {
			slog.Warn("failed to broadcast snapshot invalidation; peer replicas keep their stale snapshot until TTL",
				"topic", s.topic, "error", err)
		}
		cancel()
	}
	s.invalidate()
}
