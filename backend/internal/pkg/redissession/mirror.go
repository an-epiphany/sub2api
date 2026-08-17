package redissession

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Mirror 把「进程内存 + 共享 Redis」的 OAuth session 双写模式收敛到一处。
//
// 存在的理由：管理端加账号是两次独立的 HTTP 请求（generate-auth-url → 用户在浏览器
// 授权 → exchange-code），中间隔着数分钟的人工间隔，keep-alive 早已断开。多副本下
// 第二次请求会被负载均衡打到任意副本，session 只存进程内存时成功率退化为 1/N。
//
// 降级策略：Redis 写入失败的 session 标记为 local-only，后续读取直接走内存。
// 这样单实例部署、以及 Redis 短暂抖动时，加账号流程仍然可用（只是失去跨副本能力），
// 而不是整个功能不可用。
type Mirror[T any] struct {
	remote    *Store
	ttl       time.Duration
	createdAt func(*T) time.Time
	logName   string

	mu        sync.RWMutex
	local     map[string]*T
	localOnly map[string]struct{}
}

// NewMirror 创建一个 session 镜像。rdb 为 nil 时（未配置 Redis）退化为纯内存存储。
// createdAt 用于从会话对象里取出创建时间做 TTL 判定。
func NewMirror[T any](remote *Store, ttl time.Duration, logName string, createdAt func(*T) time.Time) *Mirror[T] {
	return &Mirror[T]{
		remote:    remote,
		ttl:       ttl,
		createdAt: createdAt,
		logName:   logName,
		local:     make(map[string]*T),
		localOnly: make(map[string]struct{}),
	}
}

func (m *Mirror[T]) Set(id string, session *T) {
	if m == nil || session == nil {
		return
	}
	var remoteErr error
	if m.remote != nil {
		remoteErr = m.remote.Set(context.Background(), id, session)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.local[id] = session
	if m.remote != nil && remoteErr != nil {
		m.localOnly[id] = struct{}{}
		slog.Warn("oauth session Redis write failed; using process-local fallback",
			"store", m.logName, "error", remoteErr)
		return
	}
	delete(m.localOnly, id)
}

func (m *Mirror[T]) Get(id string) (*T, bool) {
	if m == nil {
		return nil, false
	}
	if m.remote == nil || m.isLocalOnly(id) {
		return m.getLocal(id)
	}

	var session T
	ok, err := m.remote.Get(context.Background(), id, &session)
	if err != nil {
		// Redis 读失败时回退到本进程副本：本副本发起的授权仍能自己收尾。
		return m.getLocal(id)
	}
	if !ok {
		return nil, false
	}
	if m.expired(&session) {
		return nil, false
	}

	m.mu.Lock()
	m.local[id] = &session
	m.mu.Unlock()
	return &session, true
}

func (m *Mirror[T]) Delete(id string) {
	if m == nil {
		return
	}
	if m.remote != nil {
		_ = m.remote.Delete(context.Background(), id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.local, id)
	delete(m.localOnly, id)
}

// Sweep 清理本进程内存里已过期的 session。Redis 侧由 key TTL 自然过期。
func (m *Mirror[T]) Sweep() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, session := range m.local {
		if m.expired(session) {
			delete(m.local, id)
			delete(m.localOnly, id)
		}
	}
}

func (m *Mirror[T]) expired(session *T) bool {
	if session == nil || m.createdAt == nil {
		return false
	}
	return time.Since(m.createdAt(session)) > m.ttl
}

func (m *Mirror[T]) isLocalOnly(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.localOnly[id]
	return ok
}

func (m *Mirror[T]) getLocal(id string) (*T, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.local[id]
	if !ok || m.expired(session) {
		return nil, false
	}
	return session, true
}
