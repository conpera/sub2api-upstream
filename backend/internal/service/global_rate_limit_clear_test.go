//go:build unit

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type conditionalGlobalClearStub struct {
	*rateLimitClearRepoStub
	calls    int
	matched  bool
	observed *Account
}

func (r *conditionalGlobalClearStub) ClearGlobalRateLimitIfUnchanged(_ context.Context, observed *Account) (bool, error) {
	r.calls++
	r.observed = observed
	return r.matched, nil
}
func globalClearFixture() (*conditionalGlobalClearStub, GlobalRateLimitClearExpectation) {
	now := time.Now().UTC()
	limited, reset := now.Add(-time.Hour), now.Add(time.Hour)
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		UpdatedAt: now, RateLimitedAt: &limited, RateLimitResetAt: &reset, GroupIDs: []int64{7},
		Credentials: map[string]any{"access_token": "access", "refresh_token": "refresh", "id_token": "id"}}
	tokens, _ := json.Marshal([]string{"access", "refresh", "id"})
	hash := sha256.Sum256(tokens)
	zero := int64(0)
	sched := true
	return &conditionalGlobalClearStub{rateLimitClearRepoStub: &rateLimitClearRepoStub{getByIDAccount: account}, matched: true},
		GlobalRateLimitClearExpectation{UpdatedAt: now, LimitedAt: limited, ResetAt: reset, TokenSHA256: hex.EncodeToString(hash[:]), Status: StatusActive, Schedulable: &sched, ProxyID: &zero, GroupIDs: []int64{7}}
}
func TestConditionalGlobalClearPreservesOtherRecoveryActions(t *testing.T) {
	repo, expected := globalClearFixture()
	cache := &tempUnschedCacheRecorder{}
	svc := NewRateLimitService(repo, nil, nil, nil, cache)
	cleared, err := svc.ClearGlobalRateLimitIfUnchanged(context.Background(), 42, expected)
	require.NoError(t, err)
	require.True(t, cleared)
	require.Equal(t, 1, repo.calls)
	require.Same(t, repo.getByIDAccount, repo.observed)
	require.Zero(t, repo.clearRateLimitCalls)
	require.Zero(t, repo.clearModelRateLimitCalls)
	require.Zero(t, repo.clearTempUnschedCalls)
	require.Zero(t, repo.clearErrorCalls)
	require.Empty(t, cache.deletedIDs)
	repo.matched = false
	cleared, err = svc.ClearGlobalRateLimitIfUnchanged(context.Background(), 42, expected)
	require.NoError(t, err)
	require.False(t, cleared)
}
func TestConditionalGlobalClearRejectsChangedObservations(t *testing.T) {
	for name, change := range map[string]func(*Account){
		"version":           func(a *Account) { a.UpdatedAt = a.UpdatedAt.Add(time.Second) },
		"equal reset rearm": func(a *Account) { x := a.RateLimitedAt.Add(time.Second); a.RateLimitedAt = &x },
		"new reset":         func(a *Account) { x := a.RateLimitResetAt.Add(time.Second); a.RateLimitResetAt = &x },
		"token":             func(a *Account) { a.Credentials["refresh_token"] = "rotated" },
		"groups":            func(a *Account) { a.GroupIDs = []int64{95} },
		"proxy":             func(a *Account) { id := int64(3); a.ProxyID = &id },
		"manual stop":       func(a *Account) { a.Status = "inactive" },
		"scheduling":        func(a *Account) { a.Schedulable = false },
		"platform":          func(a *Account) { a.Platform = PlatformAnthropic },
		"shadow":            func(a *Account) { id := int64(1); a.ParentAccountID = &id },
	} {
		t.Run(name, func(t *testing.T) {
			repo, e := globalClearFixture()
			change(repo.getByIDAccount)
			svc := NewRateLimitService(repo, nil, nil, nil, nil)
			cleared, err := svc.ClearGlobalRateLimitIfUnchanged(context.Background(), 42, e)
			require.NoError(t, err)
			require.False(t, cleared)
			require.Zero(t, repo.calls)
		})
	}
}
func TestConditionalGlobalClearRequiresCompleteCapabilityAndGuards(t *testing.T) {
	repo, e := globalClearFixture()
	svc := NewRateLimitService(repo.rateLimitClearRepoStub, nil, nil, nil, nil)
	_, err := svc.ClearGlobalRateLimitIfUnchanged(context.Background(), 42, e)
	require.Error(t, err)
	for _, change := range []func(*GlobalRateLimitClearExpectation){
		func(e *GlobalRateLimitClearExpectation) { e.UpdatedAt = time.Time{} },
		func(e *GlobalRateLimitClearExpectation) { e.LimitedAt = time.Time{} },
		func(e *GlobalRateLimitClearExpectation) { e.ResetAt = time.Time{} },
		func(e *GlobalRateLimitClearExpectation) { e.TokenSHA256 = "bad" },
		func(e *GlobalRateLimitClearExpectation) { e.Schedulable = nil },
		func(e *GlobalRateLimitClearExpectation) { e.ProxyID = nil },
		func(e *GlobalRateLimitClearExpectation) { e.GroupIDs = nil },
	} {
		_, expected := globalClearFixture()
		change(&expected)
		require.False(t, expected.Valid())
	}
}
