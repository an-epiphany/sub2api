//go:build integration

package repository

import (
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// ConcurrencyMultiInstanceSuite 覆盖多副本部署（K8s replicas>1 / compose scale）下的
// 启动清扫语义：清扫必须只回收「已死实例」遗留的槽位，绝不能碰存活同伴的在途槽位。
type ConcurrencyMultiInstanceSuite struct {
	IntegrationRedisSuite
	cache *concurrencyCache
}

func TestConcurrencyMultiInstanceSuite(t *testing.T) {
	suite.Run(t, new(ConcurrencyMultiInstanceSuite))
}

func (s *ConcurrencyMultiInstanceSuite) SetupTest() {
	s.IntegrationRedisSuite.SetupTest()
	s.cache = NewConcurrencyCache(s.rdb, testSlotTTLMinutes, int(testSlotTTL.Seconds())).(*concurrencyCache)
	// 预置一次性迁移 marker：每个测试都拿到全新的 Redis 命名空间，不置位的话
	// sweepLegacyWaitKeysOnce 会在启动清扫里先把所有等待计数 SCAN+DEL 掉，
	// 掩盖掉这里真正要验证的「等待计数按实例存活情况取舍」逻辑。
	// 生产环境该 marker 在 Redis 数据生命周期内只会置位一次，之后不再触发。
	s.RequireNoError(s.rdb.Set(s.ctx, legacyWaitSweepMarkerKey, "1", 0).Err())
}

func (s *ConcurrencyMultiInstanceSuite) slotMembers(key string) []string {
	s.T().Helper()
	members, err := s.rdb.ZRange(s.ctx, key, 0, -1).Result()
	s.RequireNoError(err)
	return members
}

func (s *ConcurrencyMultiInstanceSuite) acquireAccountSlot(accountID int64, requestID string) {
	s.T().Helper()
	acquired, err := s.cache.AcquireAccountSlot(s.ctx, accountID, 50, requestID)
	s.RequireNoError(err)
	require.True(s.T(), acquired, "acquire account slot %s", requestID)
}

func (s *ConcurrencyMultiInstanceSuite) acquireUserSlot(userID int64, requestID string) {
	s.T().Helper()
	acquired, err := s.cache.AcquireUserSlot(s.ctx, userID, 50, requestID)
	s.RequireNoError(err)
	require.True(s.T(), acquired, "acquire user slot %s", requestID)
}

// 存活同伴实例持有的在途槽位，不能被另一个实例的启动清扫删掉。
// 这是 replicas>1 下最严重的故障：每次扩容/滚动更新/重启都会架空并发上限。
func (s *ConcurrencyMultiInstanceSuite) TestCleanupStaleProcessSlots_KeepsLivePeerSlots() {
	const accountID = int64(4201)
	const userID = int64(4202)

	// 同伴实例正在服务：上报心跳并持有在途槽位。
	s.RequireNoError(s.cache.HeartbeatInstance(s.ctx, "rpeer"))
	s.acquireAccountSlot(accountID, "rpeer-1")
	s.acquireUserSlot(userID, "rpeer-1")

	// 已死实例：从未上报心跳，只留下崩溃残留。
	s.acquireAccountSlot(accountID, "rdead-1")
	s.acquireUserSlot(userID, "rdead-1")

	// 本实例启动，执行启动清扫。
	s.RequireNoError(s.cache.CleanupStaleProcessSlots(s.ctx, "rself"))

	require.Equal(s.T(), []string{"rpeer-1"}, s.slotMembers(accountSlotKey(accountID)),
		"存活同伴的账号槽位必须保留，已死实例的残留必须清掉")
	require.Equal(s.T(), []string{"rpeer-1"}, s.slotMembers(userSlotKey(userID)),
		"存活同伴的用户槽位必须保留，已死实例的残留必须清掉")
}

// 心跳过期的实例视为已死，其残留槽位应被回收。
func (s *ConcurrencyMultiInstanceSuite) TestCleanupStaleProcessSlots_RemovesSlotsOfExpiredInstance() {
	const accountID = int64(4211)

	// 同伴曾经注册过，但心跳早已停止（分数远早于存活阈值）。
	now, err := s.cache.redisUnixSeconds(s.ctx)
	s.RequireNoError(err)
	s.RequireNoError(s.rdb.ZAdd(s.ctx, instanceRegistryKey, redis.Z{
		Score:  float64(now - int64(instanceHeartbeatStaleAfter.Seconds()) - 60),
		Member: "rzombie",
	}).Err())
	s.acquireAccountSlot(accountID, "rzombie-1")

	s.RequireNoError(s.cache.CleanupStaleProcessSlots(s.ctx, "rself"))

	require.Empty(s.T(), s.slotMembers(accountSlotKey(accountID)),
		"心跳过期实例的残留槽位必须被回收")
}

// 单实例部署下语义不变：崩溃残留在重启时被立即清理（不必等 slot TTL）。
func (s *ConcurrencyMultiInstanceSuite) TestCleanupStaleProcessSlots_SingleInstanceClearsCrashResidue() {
	const accountID = int64(4221)

	// 上一代进程的残留（进程前缀每次启动都会重新随机生成）。
	s.acquireAccountSlot(accountID, "rprevgen-1")
	s.acquireAccountSlot(accountID, "rprevgen-2")

	s.RequireNoError(s.cache.CleanupStaleProcessSlots(s.ctx, "rnewgen"))

	require.Empty(s.T(), s.slotMembers(accountSlotKey(accountID)),
		"单实例重启必须立即释放上一代进程的残留槽位")
}

// 有存活同伴时，等待计数不能被清零——那是同伴正在排队的请求。
func (s *ConcurrencyMultiInstanceSuite) TestCleanupStaleProcessSlots_KeepsWaitCountersWhenPeersAlive() {
	const accountID = int64(4231)

	s.RequireNoError(s.cache.HeartbeatInstance(s.ctx, "rpeer"))
	s.acquireAccountSlot(accountID, "rpeer-1")

	for range 2 {
		ok, err := s.cache.IncrementAccountWaitCount(s.ctx, accountID, 10)
		s.RequireNoError(err)
		require.True(s.T(), ok)
	}

	s.RequireNoError(s.cache.CleanupStaleProcessSlots(s.ctx, "rself"))

	waiting, err := s.cache.GetAccountWaitingCount(s.ctx, accountID)
	s.RequireNoError(err)
	require.Equal(s.T(), 2, waiting, "存活同伴的等待计数不能被其他实例的启动清扫抹掉")
}

// 单实例重启时等待计数必须清零：等待者随进程一起消失了。
func (s *ConcurrencyMultiInstanceSuite) TestCleanupStaleProcessSlots_ClearsWaitCountersWhenSoleInstance() {
	const accountID = int64(4241)

	s.acquireAccountSlot(accountID, "rprevgen-1")
	ok, err := s.cache.IncrementAccountWaitCount(s.ctx, accountID, 10)
	s.RequireNoError(err)
	require.True(s.T(), ok)

	s.RequireNoError(s.cache.CleanupStaleProcessSlots(s.ctx, "rnewgen"))

	waiting, err := s.cache.GetAccountWaitingCount(s.ctx, accountID)
	s.RequireNoError(err)
	require.Equal(s.T(), 0, waiting, "单实例重启后等待计数必须清零")
}

// 心跳把本实例注册进注册表，并顺带剔除心跳超时的实例。
func (s *ConcurrencyMultiInstanceSuite) TestHeartbeatInstance_RegistersSelfAndEvictsStale() {
	now, err := s.cache.redisUnixSeconds(s.ctx)
	s.RequireNoError(err)
	s.RequireNoError(s.rdb.ZAdd(s.ctx, instanceRegistryKey, redis.Z{
		Score:  float64(now - int64(instanceHeartbeatStaleAfter.Seconds()) - 60),
		Member: "rzombie",
	}).Err())

	s.RequireNoError(s.cache.HeartbeatInstance(s.ctx, "rself"))

	members, err := s.rdb.ZRange(s.ctx, instanceRegistryKey, 0, -1).Result()
	s.RequireNoError(err)
	require.Equal(s.T(), []string{"rself"}, members,
		"心跳应注册本实例并剔除心跳超时的实例")

	ttl, err := s.rdb.TTL(s.ctx, instanceRegistryKey).Result()
	s.RequireNoError(err)
	require.Positive(s.T(), ttl, "注册表必须带 TTL，避免实例全部下线后永久残留")
}

// 无法解析出进程前缀的成员（不含分隔符）不参与归属判定，必须保留，
// 交给 acquire 路径上的 ZREMRANGEBYSCORE 按分数自然回收。
func (s *ConcurrencyMultiInstanceSuite) TestCleanupStaleProcessSlots_KeepsMembersWithoutInstancePrefix() {
	const accountID = int64(4251)

	s.acquireAccountSlot(accountID, "legacymember")

	s.RequireNoError(s.cache.CleanupStaleProcessSlots(s.ctx, "rself"))

	require.Equal(s.T(), []string{"legacymember"}, s.slotMembers(accountSlotKey(accountID)),
		"无前缀成员无法判定归属，不能被启动清扫删掉")
}
