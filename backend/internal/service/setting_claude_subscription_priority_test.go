package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type claudePrioritySettingsRepo struct {
	*panelRateLimitSettingRepo
	readStarted chan struct{}
	readRelease chan struct{}
	readOnce    sync.Once
	setErr      error
}

type claudePrioritySettingsGroupReader struct {
	byID  map[int64]*Group
	errBy map[int64]error
}

func (r *claudePrioritySettingsGroupReader) GetByID(_ context.Context, id int64) (*Group, error) {
	if err := r.errBy[id]; err != nil {
		return nil, err
	}
	if group := r.byID[id]; group != nil {
		return group, nil
	}
	return nil, ErrGroupNotFound
}

func (r *claudePrioritySettingsRepo) GetValue(ctx context.Context, key string) (string, error) {
	value, err := r.panelRateLimitSettingRepo.GetValue(ctx, key)
	if r.readStarted != nil {
		r.readOnce.Do(func() { close(r.readStarted); <-r.readRelease })
	}
	return value, err
}

func (r *claudePrioritySettingsRepo) Set(ctx context.Context, key, value string) error {
	if r.setErr != nil {
		return r.setErr
	}
	return r.panelRateLimitSettingRepo.Set(ctx, key, value)
}

func newClaudePrioritySettingsService(raw string) (*SettingService, *claudePrioritySettingsRepo) {
	repo := &claudePrioritySettingsRepo{panelRateLimitSettingRepo: &panelRateLimitSettingRepo{values: map[string]string{}}}
	if raw != "" {
		repo.values[SettingKeyClaudeSubscriptionPriorityGroupIDs] = raw
	}
	svc := NewSettingService(repo, &config.Config{})
	svc.SetDefaultSubscriptionGroupReader(&claudePrioritySettingsGroupReader{byID: map[int64]*Group{
		28: {ID: 28, Platform: PlatformAnthropic, Status: StatusActive},
		42: {ID: 42, Platform: PlatformComposite, Status: StatusActive},
	}})
	return svc, repo
}

func expireClaudePrioritySettings(t *testing.T, svc *SettingService) {
	t.Helper()
	old, ok := svc.claudeSubscriptionPriorityCache.Load().(*cachedClaudeSubscriptionPriority)
	require.True(t, ok)
	require.NotNil(t, old)
	next := *old
	next.expiresAt = time.Now().Add(-time.Second)
	svc.claudeSubscriptionPriorityCache.Store(&next)
}

func TestClaudeSubscriptionPrioritySettings_DefaultAndInstanceIsolation(t *testing.T) {
	svc, repo := newClaudePrioritySettingsService("")
	settings, err := svc.GetClaudeSubscriptionPriority(context.Background())
	require.NoError(t, err)
	require.True(t, settings.Supported)
	require.Equal(t, "subscription_first", settings.Strategy)
	require.Equal(t, []int64{}, settings.EnabledGroupIDs)
	require.Equal(t, []string{AccountTypeOAuth, AccountTypeSetupToken}, settings.SubscriptionAccountTypes)
	for i := 0; i < 20; i++ {
		require.False(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	}
	require.Equal(t, 1, repo.getValueCalls)
	other, _ := newClaudePrioritySettingsService("[28]")
	require.True(t, other.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	require.False(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	var nilService *SettingService
	require.False(t, nilService.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
}

func TestClaudeSubscriptionPrioritySettings_RoundTripClearAndOnlyOneKey(t *testing.T) {
	svc, repo := newClaudePrioritySettingsService("[]")
	repo.values["unrelated"] = "keep"
	require.False(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	updated, err := svc.UpdateClaudeSubscriptionPriority(context.Background(), []int64{42, 28, 42})
	require.NoError(t, err)
	require.Equal(t, []int64{28, 42}, updated.EnabledGroupIDs)
	require.Equal(t, "[28,42]", repo.values[SettingKeyClaudeSubscriptionPriorityGroupIDs])
	require.Equal(t, "keep", repo.values["unrelated"])
	require.Len(t, repo.values, 2)
	updated.EnabledGroupIDs[0] = 999
	require.True(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28), "response mutations must not corrupt immutable cache")
	read, err := svc.GetClaudeSubscriptionPriority(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int64{28, 42}, read.EnabledGroupIDs)
	cleared, err := svc.UpdateClaudeSubscriptionPriority(context.Background(), []int64{})
	require.NoError(t, err)
	require.Equal(t, []int64{}, cleared.EnabledGroupIDs)
	require.Equal(t, "[]", repo.values[SettingKeyClaudeSubscriptionPriorityGroupIDs])
	require.False(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
}

func TestClaudeSubscriptionPrioritySettings_ReadFailureClearsExpiredEnabledState(t *testing.T) {
	svc, repo := newClaudePrioritySettingsService("[28]")
	require.True(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	expireClaudePrioritySettings(t, svc)
	repo.getValueErr = errors.New("isolated settings outage")
	require.False(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	settings, err := svc.GetClaudeSubscriptionPriority(context.Background())
	require.Nil(t, settings)
	require.ErrorIs(t, err, repo.getValueErr, "API must report unavailable, not a verified empty policy")
	for i := 0; i < 20; i++ {
		require.False(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	}
	require.Equal(t, 2, repo.getValueCalls, "an outage must not query the database on every request")
	repo.getValueErr = nil
	repo.values[SettingKeyClaudeSubscriptionPriorityGroupIDs] = "[42]"
	expireClaudePrioritySettings(t, svc)
	require.True(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 42))
	require.False(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
}

func TestClaudeSubscriptionPrioritySettings_InvalidStoredValuesAreErrors(t *testing.T) {
	for _, raw := range []string{"null", "{}", "true", "[", "[1.5]", "[-1]", "[0]", "[\"28\"]"} {
		t.Run(raw, func(t *testing.T) {
			svc, _ := newClaudePrioritySettingsService(raw)
			settings, err := svc.GetClaudeSubscriptionPriority(context.Background())
			require.Error(t, err)
			require.Nil(t, settings)
			require.False(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
		})
	}
}

func TestClaudeSubscriptionPrioritySettings_ValidatesGroupsBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ids      []int64
		group    *Group
		noReader bool
		readErr  error
	}{
		{name: "zero", ids: []int64{0}}, {name: "negative", ids: []int64{-1}},
		{name: "missing", ids: []int64{99}},
		{name: "inactive", ids: []int64{99}, group: &Group{ID: 99, Platform: PlatformAnthropic, Status: StatusDisabled}},
		{name: "other_platform", ids: []int64{99}, group: &Group{ID: 99, Platform: PlatformOpenAI, Status: StatusActive}},
		{name: "no_reader", ids: []int64{99}, noReader: true},
		{name: "reader_error", ids: []int64{99}, readErr: errors.New("isolated group outage")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo := newClaudePrioritySettingsService("[28]")
			svc.defaultSubGroupReader = &claudePrioritySettingsGroupReader{byID: map[int64]*Group{99: tc.group}, errBy: map[int64]error{99: tc.readErr}}
			if tc.noReader {
				svc.defaultSubGroupReader = nil
			}
			_, err := svc.UpdateClaudeSubscriptionPriority(context.Background(), tc.ids)
			require.Error(t, err)
			require.Equal(t, "[28]", repo.values[SettingKeyClaudeSubscriptionPriorityGroupIDs])
		})
	}
}

func TestClaudeSubscriptionPrioritySettings_FailedWriteDoesNotReportSuccess(t *testing.T) {
	svc, repo := newClaudePrioritySettingsService("[28]")
	require.True(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	repo.setErr = errors.New("isolated write failure")
	settings, err := svc.UpdateClaudeSubscriptionPriority(context.Background(), []int64{})
	require.ErrorIs(t, err, repo.setErr)
	require.Nil(t, settings)
	require.True(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28), "subsequent read must reflect the unchanged stored value")
}

func TestClaudeSubscriptionPrioritySettings_ConcurrentColdReadersLoadOnce(t *testing.T) {
	svc, repo := newClaudePrioritySettingsService("[28]")
	var wg sync.WaitGroup
	results := make(chan bool, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28) }()
	}
	wg.Wait()
	close(results)
	for enabled := range results {
		require.True(t, enabled)
	}
	require.Equal(t, 1, repo.getValueCalls)
}

func TestClaudeSubscriptionPrioritySettings_OtherInstanceRefreshesAfterTTL(t *testing.T) {
	first, repo := newClaudePrioritySettingsService("[]")
	second := NewSettingService(repo, &config.Config{})
	require.False(t, second.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	_, err := first.UpdateClaudeSubscriptionPriority(context.Background(), []int64{28})
	require.NoError(t, err)
	require.True(t, first.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	expireClaudePrioritySettings(t, second)
	require.True(t, second.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	_, err = first.UpdateClaudeSubscriptionPriority(context.Background(), []int64{})
	require.NoError(t, err)
	expireClaudePrioritySettings(t, second)
	require.False(t, second.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
}

func TestClaudeSubscriptionPrioritySettings_SlowReadCannotOverwriteNewWrite(t *testing.T) {
	svc, repo := newClaudePrioritySettingsService("[]")
	repo.readStarted, repo.readRelease = make(chan struct{}), make(chan struct{})
	readDone := make(chan struct{})
	go func() { svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28); close(readDone) }()
	<-repo.readStarted
	writeDone := make(chan error, 1)
	go func() {
		_, err := svc.UpdateClaudeSubscriptionPriority(context.Background(), []int64{28})
		writeDone <- err
	}()
	close(repo.readRelease)
	<-readDone
	require.NoError(t, <-writeDone)
	require.True(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	settings, err := svc.GetClaudeSubscriptionPriority(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int64{28}, settings.EnabledGroupIDs)
}
