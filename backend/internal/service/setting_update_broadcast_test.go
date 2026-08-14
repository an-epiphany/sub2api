//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// settingBroadcastRepoStub 模拟「写库成功、随后回读失败」——线上最常见的顺序是
// 请求 ctx 在 SetMultiple 落盘之后才超时，紧接着的 GetAll 直接失败。
type settingBroadcastRepoStub struct {
	getAllErr error
	updates   map[string]string
}

func (s *settingBroadcastRepoStub) Get(context.Context, string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *settingBroadcastRepoStub) GetValue(context.Context, string) (string, error) {
	panic("unexpected GetValue call")
}

func (s *settingBroadcastRepoStub) Set(context.Context, string, string) error {
	panic("unexpected Set call")
}

func (s *settingBroadcastRepoStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *settingBroadcastRepoStub) SetMultiple(_ context.Context, settings map[string]string) error {
	s.updates = make(map[string]string, len(settings))
	for k, v := range settings {
		s.updates[k] = v
	}
	return nil
}

func (s *settingBroadcastRepoStub) GetAll(context.Context) (map[string]string, error) {
	return nil, s.getAllErr
}

func (s *settingBroadcastRepoStub) Delete(context.Context, string) error {
	panic("unexpected Delete call")
}

// 部分更新会回读一次数据库来重建本副本快照。回读失败是本副本自己的问题，
// 写入已经落盘，此时跳过广播会让每个同伴副本继续用旧设置直到各自 TTL（约 60 秒）过期。
func TestSettingService_PartialUpdateBroadcastsEvenWhenRereadFails(t *testing.T) {
	repo := &settingBroadcastRepoStub{getAllErr: errors.New("read timeout after write")}
	svc := NewSettingService(repo, &config.Config{})

	cache := newFakeSnapshotInvalidationCache()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc.SetSnapshotInvalidationCache(ctx, cache)
	require.Eventually(t, func() bool {
		return cache.subscriberCount(SnapshotTopicSettings) == 1
	}, time.Second, 5*time.Millisecond, "设置副本应订阅失效广播")

	err := svc.UpdateSettingsOmitting(
		context.Background(),
		&SystemSettings{CompactHomeEnabled: true},
		OmittedSettingKeys{SettingKeySiteName: {}},
	)

	require.NoError(t, err)
	require.Equal(t, "true", repo.updates[SettingKeyCompactHomeEnabled], "写库必须已经发生")
	require.EqualValues(t, 1, cache.publishCount(SnapshotTopicSettings),
		"本副本回读失败不能连累同伴一起陈旧")
}
