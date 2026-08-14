//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/channelmonitor"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// createClaimTestMonitor 建一条最小可用的监控记录。
func createClaimTestMonitor(t *testing.T, ctx context.Context, client *dbent.Client, name string, enabled bool) int64 {
	t.Helper()
	m, err := client.ChannelMonitor.Create().
		SetName(name).
		SetProvider(channelmonitor.ProviderOpenai).
		SetAPIMode(service.MonitorAPIModeResponses).
		SetEndpoint("https://api.example.com").
		SetAPIKeyEncrypted("encrypted-key").
		SetPrimaryModel("gpt-5.4-mini").
		SetIntervalSeconds(300).
		SetEnabled(enabled).
		SetCreatedBy(1).
		Save(ctx)
	require.NoError(t, err)
	return m.ID
}

// 触发窗口声明是「同一轮只探一次」的唯一闸门：多副本的定时器相位不同，
// 谁先到点谁探，其余副本必须在同一个窗口内被数据库直接拒绝。
func TestChannelMonitorTryClaimCheck_OneWinnerPerWindow(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()
	repo := NewChannelMonitorRepository(integrationEntClient, integrationDB)

	id := createClaimTestMonitor(t, ctx, client, "claim-window", true)
	const window = 5 * time.Minute

	first := time.Now()
	claimed, err := repo.TryClaimCheck(ctx, id, first.Add(-window), first)
	require.NoError(t, err)
	require.True(t, claimed, "从未检测过的监控必须能声明成功")

	// 同伴副本 5 秒后到点：窗口未过，必须被拒。
	peerAt := first.Add(5 * time.Second)
	claimed, err = repo.TryClaimCheck(ctx, id, peerAt.Add(-window), peerAt)
	require.NoError(t, err)
	require.False(t, claimed, "窗口内的第二个副本不得再探测同一个上游")

	stored, err := client.ChannelMonitor.Get(ctx, id)
	require.NoError(t, err)
	require.WithinDuration(t, first, *stored.LastCheckedAt, time.Second,
		"被拒的副本不能推进 last_checked_at，否则窗口会被无限续期")

	// 下一轮窗口到了，任意副本都可以接手。
	nextAt := first.Add(window + time.Second)
	claimed, err = repo.TryClaimCheck(ctx, id, nextAt.Add(-window), nextAt)
	require.NoError(t, err)
	require.True(t, claimed, "窗口过去后必须恢复探测，不能被永久卡住")
}

// 停用/删除只在处理该请求的副本上取消定时器，同伴要到重启才知道。
// 把 enabled 写进声明条件，同伴的残留定时器就自然探不动了。
func TestChannelMonitorTryClaimCheck_RejectsDisabledOrMissingMonitor(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()
	repo := NewChannelMonitorRepository(integrationEntClient, integrationDB)

	disabledID := createClaimTestMonitor(t, ctx, client, "claim-disabled", false)

	now := time.Now()
	claimed, err := repo.TryClaimCheck(ctx, disabledID, now.Add(-time.Minute), now)
	require.NoError(t, err)
	require.False(t, claimed, "已停用的监控不得被同伴的残留定时器探测")

	claimed, err = repo.TryClaimCheck(ctx, disabledID+100000, now.Add(-time.Minute), now)
	require.NoError(t, err)
	require.False(t, claimed, "记录已删除时声明失败即可，不应报错")
}
