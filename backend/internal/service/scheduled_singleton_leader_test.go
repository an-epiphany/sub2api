//go:build unit

package service

import (
	"context"
	"encoding/json"
	"sync"
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

// 恢复必须按自己的执行者和开始时间判活：一份几小时前做的备份现在正被同伴恢复时，
// 拿备份的 StartedAt 去判，会把在跑的恢复直接标记成 failed。
func TestBackupService_RecoverStaleRecordsKeepsPeerRunningRestoreOfOldBackup(t *testing.T) {
	now := time.Now()
	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:                     "old-backup-being-restored",
		Status:                 "completed",
		OwnerInstanceID:        "peer-instance",
		StartedAt:              now.Add(-3 * time.Hour).Format(time.RFC3339), // 备份是三小时前做的
		RestoreStatus:          "running",
		RestoreOwnerInstanceID: "peer-instance",
		RestoreStartedAt:       now.Format(time.RFC3339), // 恢复刚刚开始
	}})}
	svc := newRecoveryBackupService(repo)

	svc.recoverStaleRecords()

	require.Zero(t, repo.setCalls.Load(), "同伴正在跑的恢复不得被标记为 failed")
}

// 同伴的恢复挂了很久同样要回收，否则记录永远停在 running。
func TestBackupService_RecoverStaleRecordsReclaimsLongDeadPeerRestore(t *testing.T) {
	now := time.Now()
	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:                     "dead-peer-restore",
		Status:                 "completed",
		OwnerInstanceID:        "peer-instance",
		StartedAt:              now.Add(-3 * time.Hour).Format(time.RFC3339),
		RestoreStatus:          "running",
		RestoreOwnerInstanceID: "peer-instance",
		RestoreStartedAt:       now.Add(-backupPeerRecordStaleAfter - time.Hour).Format(time.RFC3339),
	}})}
	svc := newRecoveryBackupService(repo)

	svc.recoverStaleRecords()

	require.Positive(t, repo.setCalls.Load(), "长期未收尾的同伴恢复应被回收")
}

// 升级前写下的记录没有恢复执行者字段，沿用旧语义立即回收。
func TestBackupService_RecoverStaleRecordsReclaimsLegacyRunningRestore(t *testing.T) {
	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:            "legacy-restore",
		Status:        "completed",
		StartedAt:     time.Now().Format(time.RFC3339),
		RestoreStatus: "running",
	}})}
	svc := newRecoveryBackupService(repo)

	svc.recoverStaleRecords()

	require.Positive(t, repo.setCalls.Load(), "无恢复 owner 的历史记录应立即回收")
}

// 周期复检是跨实例的清扫，必须选主：备份记录是 settings 里的单行 JSON，
// 多个实例同时读-改-写会互相覆盖，还会对同一批 S3 对象重复发删除。
func TestBackupService_RecoverySweepSkipsWhenPeerHoldsLeaderLock(t *testing.T) {
	lock := &fakeLeaderLockCache{}
	_, err := lock.TryAcquireLeaderLock(context.Background(), backupRecoveryLeaderLockKey, "peer-instance", time.Minute)
	require.NoError(t, err)

	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:        "legacy-backup",
		Status:    "running",
		StartedAt: time.Now().Format(time.RFC3339),
	}})}
	svc := newRecoveryBackupService(repo)
	svc.SetLeaderLock(lock, nil)

	svc.recoverStaleRecordsAsLeader()

	require.Zero(t, repo.getCalls.Load(), "同伴持锁时不得读取备份记录")
	require.Equal(t, "peer-instance", lock.heldBy(backupRecoveryLeaderLockKey), "不得抢走同伴的锁")
}

// 拿到锁时正常复检并释放锁：阈值到达的那一刻通常没有任何启动事件，
// 没有这一轮复检，记录会永远卡在 running，S3 产物也永远没人清理。
func TestBackupService_RecoverySweepRunsAndReleasesLockWhenLeader(t *testing.T) {
	lock := &fakeLeaderLockCache{}
	repo := &stubBackupSettingRepo{value: backupRecordsJSON(t, []BackupRecord{{
		ID:              "dead-peer-backup",
		Status:          "running",
		OwnerInstanceID: "peer-instance",
		StartedAt:       time.Now().Add(-backupPeerRecordStaleAfter - time.Hour).Format(time.RFC3339),
	}})}
	svc := newRecoveryBackupService(repo)
	svc.SetLeaderLock(lock, nil)

	svc.recoverStaleRecordsAsLeader()

	require.Positive(t, repo.setCalls.Load(), "leader 必须真正执行复检回收")
	require.Empty(t, lock.heldBy(backupRecoveryLeaderLockKey), "复检结束必须释放锁")
}

// ── ChannelMonitorRunner ──────────────────────────────────────────────────────

// monitorTask 构造一个等价于 Schedule 出来的调度任务，供直接驱动 runOne 使用。
func monitorTask(id int64, name string, interval time.Duration) *scheduledMonitor {
	return &scheduledMonitor{id: id, name: name, interval: interval}
}

// 同伴实例持有某个 monitor 的 leader lock 时，本实例这一次触发必须跳过。
// 这把锁只防重叠：同一轮窗口的去重由 TryClaimScheduledCheck 负责。
func TestChannelMonitorRunner_SkipsCheckWhenPeerHoldsLeaderLock(t *testing.T) {
	const monitorID = int64(77)

	lock := &fakeLeaderLockCache{}
	_, err := lock.TryAcquireLeaderLock(context.Background(), channelMonitorFireLeaderLockKey(monitorID), "peer-instance", time.Minute)
	require.NoError(t, err)

	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	runner := newRunnerForTest(svc)
	runner.SetLeaderLock(lock, nil)

	runner.runOne(monitorTask(monitorID, "peer-owned", time.Minute))

	require.Zero(t, svc.runCount.Load(), "同伴持锁时不得发起检测")
	require.Zero(t, svc.claimCount.Load(), "没抢到锁就不该推进触发窗口——白吃掉一轮探测")
	require.Equal(t, "peer-instance", lock.heldBy(channelMonitorFireLeaderLockKey(monitorID)), "不得抢走同伴的锁")
}

// 拿到锁的实例正常检测，并在结束后释放锁，让下一轮由任意实例接管。
func TestChannelMonitorRunner_RunsAndReleasesLockWhenLeader(t *testing.T) {
	const monitorID = int64(78)

	lock := &fakeLeaderLockCache{}
	svc := &stubMonitorSvc{runCalled: make(chan int64, 4)}
	runner := newRunnerForTest(svc)
	runner.SetLeaderLock(lock, nil)

	runner.runOne(monitorTask(monitorID, "self-owned", time.Minute))

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

	runner.runOne(monitorTask(102, "other-monitor", time.Minute))

	require.EqualValues(t, 1, svc.runCount.Load(), "同伴持有的是另一个 monitor 的锁，不应阻塞本 monitor")
}

// sharedWindowMonitorSvc 用一份共享的 last-checked 时间模拟数据库上的窗口声明，
// 让两个副本像生产一样竞争同一行 channel_monitors。
type sharedWindowMonitorSvc struct {
	mu          sync.Mutex
	lastChecked time.Time
	runs        atomic.Int64
	now         func() time.Time
}

func (s *sharedWindowMonitorSvc) ListEnabledMonitors(context.Context) ([]*ChannelMonitor, error) {
	return nil, nil
}

func (s *sharedWindowMonitorSvc) RunCheck(_ context.Context, _ int64) ([]*CheckResult, error) {
	s.runs.Add(1)
	return nil, nil
}

func (s *sharedWindowMonitorSvc) TryClaimScheduledCheck(_ context.Context, _ int64, window time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !s.lastChecked.IsZero() && now.Sub(s.lastChecked) < window {
		return false, nil
	}
	s.lastChecked = now
	return true, nil
}

// 各副本的定时器互相独立，相位差是常态：A 跑完释放锁，几秒后 B 的定时器到点，
// 于是同一轮里同一个上游被探测两次——锁只挡重叠，挡不住这个。
// 触发窗口声明才是这件事的闸门。
func TestChannelMonitorRunner_WindowClaimStopsStaggeredPeerProbes(t *testing.T) {
	clock := time.Now()
	svc := &sharedWindowMonitorSvc{now: func() time.Time { return clock }}
	lock := &fakeLeaderLockCache{}

	replicaA := newRunnerForTest(svc)
	replicaA.SetLeaderLock(lock, nil)
	replicaB := newRunnerForTest(svc)
	replicaB.SetLeaderLock(lock, nil)

	task := monitorTask(79, "m79", 5*time.Minute)

	replicaA.runOne(task)
	clock = clock.Add(5 * time.Second) // B 的定时器晚 5 秒到点，此时锁早已释放
	replicaB.runOne(task)

	require.EqualValues(t, 1, svc.runs.Load(), "同一轮触发窗口内只能有一个副本真正探测")

	clock = clock.Add(5 * time.Minute) // 进入下一轮窗口
	replicaB.runOne(task)

	require.EqualValues(t, 2, svc.runs.Load(), "窗口过去后必须继续按节奏探测，不能被永久卡住")
}
