//go:build unit

package service

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 这一组测试守护「周期性任务在多副本下只能有一个实例真正执行」这个不变式。
// 判据统一为：让同伴实例先持有 leader lock，然后确认本实例的执行路径没有
// 产生任何外部可见的副作用（不查库、不打上游、不写记录）。

// ── ScheduledTestRunnerService ────────────────────────────────────────────────

type stubScheduledTestPlanRepo struct {
	ScheduledTestPlanRepository
	listDueCalls atomic.Int64
}

func (r *stubScheduledTestPlanRepo) ListDue(_ context.Context, _ time.Time) ([]*ScheduledTestPlan, error) {
	r.listDueCalls.Add(1)
	return nil, nil
}

// 同伴实例持有 leader lock 时，本实例这一轮必须整轮跳过。
// 否则每个到期计划会被 N 个副本各跑一次，直接产生 N 倍的真实上游计费请求。
func TestScheduledTestRunner_SkipsWhenPeerHoldsLeaderLock(t *testing.T) {
	lock := &fakeLeaderLockCache{}
	_, err := lock.TryAcquireLeaderLock(context.Background(), scheduledTestRunnerLeaderLockKey, "peer-instance", time.Minute)
	require.NoError(t, err)

	repo := &stubScheduledTestPlanRepo{}
	svc := &ScheduledTestRunnerService{planRepo: repo, instanceID: "self-instance"}
	svc.SetLeaderLock(lock, nil)

	svc.runScheduledOnce()

	require.Zero(t, repo.listDueCalls.Load(), "同伴持锁时不得查询到期计划")
	require.Equal(t, "peer-instance", lock.heldBy(scheduledTestRunnerLeaderLockKey), "不得抢走同伴的锁")
}

// 拿到 leader lock 时正常执行，并且执行完必须释放锁，让下一轮重新竞选。
func TestScheduledTestRunner_RunsAndReleasesLockWhenLeader(t *testing.T) {
	lock := &fakeLeaderLockCache{}
	repo := &stubScheduledTestPlanRepo{}
	svc := &ScheduledTestRunnerService{planRepo: repo, instanceID: "self-instance"}
	svc.SetLeaderLock(lock, nil)

	svc.runScheduledOnce()

	require.EqualValues(t, 1, repo.listDueCalls.Load(), "leader 必须真正执行本轮")
	require.Empty(t, lock.heldBy(scheduledTestRunnerLeaderLockKey), "执行结束必须释放锁")
}

// ── BackupService ─────────────────────────────────────────────────────────────

type stubBackupSettingRepo struct {
	SettingRepository
	value    string
	getCalls atomic.Int64
	setCalls atomic.Int64
}

func (r *stubBackupSettingRepo) GetValue(_ context.Context, _ string) (string, error) {
	r.getCalls.Add(1)
	return r.value, nil
}

func (r *stubBackupSettingRepo) Set(_ context.Context, _, value string) error {
	r.setCalls.Add(1)
	r.value = value
	return nil
}

func backupRecordsJSON(t *testing.T, records []BackupRecord) string {
	t.Helper()
	raw, err := json.Marshal(records)
	require.NoError(t, err)
	return string(raw)
}

func newRecoveryBackupService(repo SettingRepository) *BackupService {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &BackupService{settingRepo: repo, bgCtx: ctx, bgCancel: cancel, instanceID: "self-instance"}
	return svc
}

// 同伴实例持有 leader lock 时，本实例不得启动定时备份。
// 否则 N 个副本会在同一个 cron tick 并发跑 N 次全库 pg_dump，
// 并且备份记录（settings 单行 JSON 的读-改-写）会互相覆盖成孤儿对象。
func TestBackupService_ScheduledBackupSkipsWhenPeerHoldsLeaderLock(t *testing.T) {
	lock := &fakeLeaderLockCache{}
	_, err := lock.TryAcquireLeaderLock(context.Background(), backupScheduledLeaderLockKey, "peer-instance", time.Minute)
	require.NoError(t, err)

	settingRepo := &stubBackupSettingRepo{}
	svc := newRecoveryBackupService(settingRepo)
	svc.SetLeaderLock(lock, nil)

	svc.runScheduledBackup()

	require.Zero(t, settingRepo.getCalls.Load(), "同伴持锁时不得读取备份配置，更不能开始备份")
	require.Equal(t, "peer-instance", lock.heldBy(backupScheduledLeaderLockKey), "不得抢走同伴的锁")
}

// ── BackupService: 启动时的 running 记录回收 ───────────────────────────────────

// 同伴实例正在跑的备份，不能被本实例的启动回收标记为 failed 并删掉它的 S3 对象。
// 这是数据销毁级别的问题：对方备份完成时记录已被改写，产物成为无人认领的孤儿。
func TestBackupService_RecoverStaleRecordsKeepsPeerRunningRecord(t *testing.T) {
	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:              "peer-backup",
		Status:          "running",
		OwnerInstanceID: "peer-instance",
		StartedAt:       time.Now().Format(time.RFC3339),
	}})}
	svc := newRecoveryBackupService(repo)

	svc.recoverStaleRecords()

	require.Zero(t, repo.setCalls.Load(), "同伴正在跑的备份记录不得被改写")
}

// 本实例上一代进程留下的 running 记录必须立即回收（单实例重启的原有语义）。
func TestBackupService_RecoverStaleRecordsReclaimsOwnResidue(t *testing.T) {
	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:              "own-backup",
		Status:          "running",
		OwnerInstanceID: "self-instance",
		StartedAt:       time.Now().Format(time.RFC3339),
	}})}
	svc := newRecoveryBackupService(repo)

	svc.recoverStaleRecords()

	require.Positive(t, repo.setCalls.Load(), "本实例自己的崩溃残留必须立即回收")
}

// 升级前写下的旧记录没有 owner 字段，沿用旧语义立即回收，避免它们永远卡在 running。
func TestBackupService_RecoverStaleRecordsReclaimsLegacyRecordWithoutOwner(t *testing.T) {
	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:        "legacy-backup",
		Status:    "running",
		StartedAt: time.Now().Format(time.RFC3339),
	}})}
	svc := newRecoveryBackupService(repo)

	svc.recoverStaleRecords()

	require.Positive(t, repo.setCalls.Load(), "无 owner 的历史记录应立即回收")
}

// 同伴的记录挂了很久（超过任何一次备份可能的生命周期）就可以断定它已经死了。
func TestBackupService_RecoverStaleRecordsReclaimsLongDeadPeerRecord(t *testing.T) {
	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:              "dead-peer-backup",
		Status:          "running",
		OwnerInstanceID: "peer-instance",
		StartedAt:       time.Now().Add(-backupPeerRecordStaleAfter - time.Hour).Format(time.RFC3339),
	}})}
	svc := newRecoveryBackupService(repo)

	svc.recoverStaleRecords()

	require.Positive(t, repo.setCalls.Load(), "长期未收尾的同伴记录应被回收")
}

// StartedAt 无法解析时按「不确定」处理：宁可让记录多挂一会儿，也不能误删他人产物。
func TestBackupService_RecoverStaleRecordsKeepsPeerRecordWithUnparsableStartedAt(t *testing.T) {
	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:              "weird-backup",
		Status:          "running",
		OwnerInstanceID: "peer-instance",
		StartedAt:       "not-a-timestamp",
	}})}
	svc := newRecoveryBackupService(repo)

	svc.recoverStaleRecords()

	require.Zero(t, repo.setCalls.Load(), "时间无法解析时不得回收同伴记录")
}

// ── ChannelMonitorRunner ──────────────────────────────────────────────────────

// 同伴实例持有某个 monitor 的 leader lock 时，本实例这一次触发必须跳过。
// 否则同一个监控会被 N 个副本各探测一次，上游压力和历史记录都放大 N 倍。
func TestChannelMonitorRunner_SkipsCheckWhenPeerHoldsLeaderLock(t *testing.T) {
	const monitorID = int64(77)

	lock := &fakeLeaderLockCache{}
	_, err := lock.TryAcquireLeaderLock(context.Background(), channelMonitorFireLeaderLockKey(monitorID), "peer-instance", time.Minute)
	require.NoError(t, err)

	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	runner := newRunnerForTest(svc)
	runner.SetLeaderLock(lock, nil)

	runner.runOne(monitorID, "peer-owned")

	require.Zero(t, svc.runCount.Load(), "同伴持锁时不得发起检测")
	require.Equal(t, "peer-instance", lock.heldBy(channelMonitorFireLeaderLockKey(monitorID)), "不得抢走同伴的锁")
}

// 拿到锁的实例正常检测，并在结束后释放锁，让下一轮由任意实例接管。
func TestChannelMonitorRunner_RunsAndReleasesLockWhenLeader(t *testing.T) {
	const monitorID = int64(78)

	lock := &fakeLeaderLockCache{}
	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	runner := newRunnerForTest(svc)
	runner.SetLeaderLock(lock, nil)

	runner.runOne(monitorID, "self-owned")

	require.EqualValues(t, 1, svc.runCount.Load(), "leader 必须真正执行检测")
	require.Empty(t, lock.heldBy(channelMonitorFireLeaderLockKey(monitorID)), "检测结束必须释放锁")
}

// 每个 monitor 用独立的锁：不同监控可以分散到不同副本上并行跑，
// 而不是被单一 leader 串行化。
func TestChannelMonitorRunner_LeaderLockIsPerMonitor(t *testing.T) {
	lock := &fakeLeaderLockCache{}
	_, err := lock.TryAcquireLeaderLock(context.Background(), channelMonitorFireLeaderLockKey(101), "peer-instance", time.Minute)
	require.NoError(t, err)

	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	runner := newRunnerForTest(svc)
	runner.SetLeaderLock(lock, nil)

	runner.runOne(102, "other-monitor")

	require.EqualValues(t, 1, svc.runCount.Load(), "同伴持有的是另一个 monitor 的锁，不应阻塞本 monitor")
}
