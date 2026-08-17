package setup

import (
	"errors"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 多副本冷启动时，多个实例会同时通过「COUNT users == 0」这道检查，然后一起去建管理员。
// users.email 上有部分唯一索引，因此只有一个能成功；输家必须把这件事识别为
// 「别人已经建好了」，而不是当成致命错误退出（AutoSetupFromEnv 的错误会 log.Fatalf）。

func TestAdminBootstrapOutcome_InsertedByThisInstance(t *testing.T) {
	created, reason := adminBootstrapOutcome(1, adminBootstrapReasonEmptyDatabase)
	require.True(t, created, "本实例插入成功时应报告已创建")
	require.Equal(t, adminBootstrapReasonEmptyDatabase, reason)
}

func TestAdminBootstrapOutcome_LostRaceToPeerInstance(t *testing.T) {
	created, reason := adminBootstrapOutcome(0, adminBootstrapReasonEmptyDatabase)
	require.False(t, created, "被同伴抢先时不应报告为本实例创建")
	require.Equal(t, adminBootstrapReasonAdminExists, reason,
		"输掉竞态等价于「管理员已存在」，冷启动必须继续而不是 Fatal 退出")
}

// CREATE DATABASE 同样是 check-then-act：目标库不存在时多个实例会同时创建，
// 输家会收到 42P04 duplicate_database。那说明库已经就绪，不是失败。
func TestIsDuplicateDatabaseError(t *testing.T) {
	require.True(t, isDuplicateDatabaseError(&pq.Error{Code: "42P04"}),
		"42P04 duplicate_database 说明另一个实例已经建好了库")
	require.False(t, isDuplicateDatabaseError(&pq.Error{Code: "42501"}),
		"权限不足是真失败，不能吞掉")
	require.False(t, isDuplicateDatabaseError(errors.New("connection refused")))
	require.False(t, isDuplicateDatabaseError(nil))
}
