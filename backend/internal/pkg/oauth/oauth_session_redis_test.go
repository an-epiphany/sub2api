//go:build unit

package oauth

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// 管理端加账号是两次独立的 HTTP 请求（generate-auth-url → 浏览器授权 → exchange-code）。
// 多副本下第二次请求会被负载均衡打到任意副本，session 只存进程内存时成功率退化为 1/N。
// 因此 session 必须落在共享存储上。

func claudeNewRedisTestStore(t *testing.T) (*SessionStore, *SessionStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	instanceA := NewRedisSessionStore(rdb)
	t.Cleanup(instanceA.Stop)
	instanceB := NewRedisSessionStore(rdb)
	t.Cleanup(instanceB.Stop)
	return instanceA, instanceB, mr
}

func TestSessionStore_RedisSessionVisibleAcrossInstances(t *testing.T) {
	instanceA, instanceB, _ := claudeNewRedisTestStore(t)

	instanceA.Set("sess-1", &OAuthSession{State: "st-1", CodeVerifier: "verifier-1", CreatedAt: time.Now()})

	got, ok := instanceB.Get("sess-1")
	require.True(t, ok, "另一个副本必须能读到同一个 session")
	require.Equal(t, "verifier-1", got.CodeVerifier)
	require.Equal(t, "st-1", got.State)
}

func TestSessionStore_RedisDeletePropagatesAcrossInstances(t *testing.T) {
	instanceA, instanceB, _ := claudeNewRedisTestStore(t)

	instanceA.Set("sess-2", &OAuthSession{State: "st-2", CodeVerifier: "verifier-2", CreatedAt: time.Now()})
	require.True(t, func() bool { _, ok := instanceB.Get("sess-2"); return ok }())

	instanceA.Delete("sess-2")

	_, ok := instanceB.Get("sess-2")
	require.False(t, ok, "一个副本删除后其他副本也不应再读到")
}

func TestSessionStore_RedisSessionExpiresByTTL(t *testing.T) {
	instanceA, instanceB, _ := claudeNewRedisTestStore(t)

	instanceA.Set("sess-3", &OAuthSession{State: "st-3", CodeVerifier: "v3", CreatedAt: time.Now().Add(-SessionTTL - time.Minute)})

	_, ok := instanceB.Get("sess-3")
	require.False(t, ok, "超过 TTL 的 session 不应被接受")
}

// Redis 不可用时必须降级为进程内存：单实例部署和 Redis 抖动都不该让加账号流程整个失效。
func TestSessionStore_FallsBackToMemoryWhenRedisUnavailable(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := NewRedisSessionStore(rdb)
	t.Cleanup(store.Stop)
	mr.Close()

	store.Set("sess-4", &OAuthSession{State: "st-4", CodeVerifier: "v4", CreatedAt: time.Now()})

	got, ok := store.Get("sess-4")
	require.True(t, ok, "Redis 不可用时应降级为进程内存")
	require.Equal(t, "v4", got.CodeVerifier)
}

// 不注入 Redis 时保持原有的纯内存行为。
func TestSessionStore_MemoryOnlyWithoutRedis(t *testing.T) {
	store := NewSessionStore()
	t.Cleanup(store.Stop)

	store.Set("sess-5", &OAuthSession{State: "st-5", CodeVerifier: "v5", CreatedAt: time.Now()})

	got, ok := store.Get("sess-5")
	require.True(t, ok)
	require.Equal(t, "v5", got.CodeVerifier)
}
