//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

const claudeSubscriptionModel = "claude-sonnet-4-5"

// Record actual slot attempts: load snapshots alone cannot prove a pool is full.
type claudeSubscriptionConcurrencyCache struct {
	*mockConcurrencyCache
	attempts   []int64
	released   []int64
	acquireErr map[int64]error
}

func (c *claudeSubscriptionConcurrencyCache) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	c.attempts = append(c.attempts, accountID)
	if err := c.acquireErr[accountID]; err != nil {
		return false, err
	}
	return c.mockConcurrencyCache.AcquireAccountSlot(ctx, accountID, maxConcurrency, requestID)
}

func (c *claudeSubscriptionConcurrencyCache) ReleaseAccountSlot(_ context.Context, accountID int64, _ string) error {
	c.released = append(c.released, accountID)
	return nil
}

type claudeSubscriptionSessionCache struct {
	SessionLimitCache
	registered []int64
	denied     map[int64]bool
}

func (c *claudeSubscriptionSessionCache) RegisterSession(_ context.Context, accountID int64, _ string, _ int, _ time.Duration) (bool, error) {
	if c.denied[accountID] {
		return false, nil
	}
	c.registered = append(c.registered, accountID)
	return true, nil
}

type claudeSubscriptionFixture struct {
	svc         *GatewayService
	group       *Group
	repo        *mockAccountRepoForPlatform
	sticky      *mockGatewayCacheForPlatform
	concurrency *claudeSubscriptionConcurrencyCache
}

func claudeSubscriptionAccount(id int64, accountType string, priority int) Account {
	return Account{
		ID: id, Platform: PlatformAnthropic, Type: accountType,
		Priority: priority, Status: StatusActive, Schedulable: true, Concurrency: 10,
		AccountGroups: []AccountGroup{{GroupID: 42}},
	}
}

func newClaudeSubscriptionFixture(accounts ...Account) *claudeSubscriptionFixture {
	repo := &mockAccountRepoForPlatform{accounts: accounts, accountsByID: make(map[int64]*Account)}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	group := &Group{ID: 42, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true}
	sticky := &mockGatewayCacheForPlatform{sessionBindings: make(map[string]int64)}
	concurrency := &claudeSubscriptionConcurrencyCache{mockConcurrencyCache: &mockConcurrencyCache{
		acquireResults: make(map[int64]bool),
		loadMap:        make(map[int64]*AccountLoadInfo),
	}}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 10
	return &claudeSubscriptionFixture{
		group: group, repo: repo, sticky: sticky, concurrency: concurrency,
		svc: &GatewayService{
			accountRepo: repo, groupRepo: &mockGroupRepoForGateway{groups: map[int64]*Group{group.ID: group}},
			cache: sticky, cfg: cfg, concurrencyService: NewConcurrencyService(concurrency),
			settingService: NewSettingService(&betaPolicySettingRepoStub{values: map[string]string{
				SettingKeyClaudeSubscriptionPriorityGroupIDs: "[42]",
			}}, cfg),
		},
	}
}

func (f *claudeSubscriptionFixture) route(ids ...int64) {
	f.group.ModelRoutingEnabled = true
	f.group.ModelRouting = map[string][]int64{claudeSubscriptionModel: ids}
}

func (f *claudeSubscriptionFixture) selectAccount(t *testing.T, session string, excluded map[int64]struct{}) *AccountSelectionResult {
	t.Helper()
	result, err := f.svc.SelectAccountWithLoadAwareness(context.Background(), &f.group.ID, session, claudeSubscriptionModel, excluded, "", 0)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
	if result.ReleaseFunc != nil {
		t.Cleanup(result.ReleaseFunc)
	}
	return result
}

func TestClaudeSubscriptionPriority_PrecedesRegularPriorityStickyAndRoute(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		for _, preference := range []string{"priority", "sticky", "model_route", "routed_sticky"} {
			t.Run(accountType+"/"+preference, func(t *testing.T) {
				f := newClaudeSubscriptionFixture(
					claudeSubscriptionAccount(1, accountType, 100),
					claudeSubscriptionAccount(2, AccountTypeAPIKey, 1),
				)
				if preference == "sticky" || preference == "routed_sticky" {
					f.sticky.sessionBindings["session"] = 2
				}
				if preference == "model_route" || preference == "routed_sticky" {
					f.route(2)
				}
				result := f.selectAccount(t, "session", nil)
				require.Equal(t, int64(1), result.Account.ID, "an available Claude subscription must precede a regular account")
				require.True(t, result.Acquired)
				require.Nil(t, result.WaitPlan)
				require.NotContains(t, f.concurrency.attempts, int64(2), "do not reserve a regular slot while the subscription pool is available")
			})
		}
	}
}

func TestClaudeSubscriptionPriority_PreservesPreferencesWithinPool(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sticky int64
		route  []int64
		want   int64
	}{
		{name: "numeric_priority", want: 1},
		{name: "sticky_precedes_priority", sticky: 2, want: 2},
		{name: "route_precedes_sticky", sticky: 1, route: []int64{2}, want: 2},
		{name: "sticky_within_route", sticky: 2, route: []int64{1, 2}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaudeSubscriptionFixture(
				claudeSubscriptionAccount(1, AccountTypeOAuth, 10),
				claudeSubscriptionAccount(2, AccountTypeSetupToken, 20),
				claudeSubscriptionAccount(3, AccountTypeAPIKey, 1),
			)
			f.sticky.sessionBindings["session"] = tc.sticky
			if len(tc.route) > 0 {
				f.route(tc.route...)
			}
			result := f.selectAccount(t, "session", nil)
			require.Equal(t, tc.want, result.Account.ID)
			require.True(t, result.Acquired)
		})
	}
}

func TestClaudeSubscriptionPriority_ExhaustsSubscriptionPoolBeforeSpillover(t *testing.T) {
	for _, subscriptionAvailable := range []bool{true, false} {
		name := "last_subscription_has_slot"
		if !subscriptionAvailable {
			name = "all_subscriptions_full"
		}
		t.Run(name, func(t *testing.T) {
			accounts := make([]Account, 0, 13)
			for id := int64(1); id <= 12; id++ {
				accounts = append(accounts, claudeSubscriptionAccount(id, AccountTypeOAuth, 100+int(id)))
			}
			accounts = append(accounts, claudeSubscriptionAccount(100, AccountTypeAPIKey, 1))
			f := newClaudeSubscriptionFixture(accounts...)
			for id := int64(1); id <= 12; id++ {
				f.concurrency.acquireResults[id] = false
			}
			f.concurrency.acquireResults[12] = subscriptionAvailable
			result := f.selectAccount(t, "", nil)
			wantID := int64(100)
			if subscriptionAvailable {
				wantID = 12
			}
			require.Equal(t, wantID, result.Account.ID)
			require.True(t, result.Acquired)
			require.Nil(t, result.WaitPlan)
			for id := int64(1); id <= 12; id++ {
				require.Contains(t, f.concurrency.attempts, id, "every eligible subscription must be tried, including accounts beyond a top-K shortlist")
			}
			if subscriptionAvailable {
				require.NotContains(t, f.concurrency.attempts, int64(100))
			} else {
				require.Equal(t, int64(100), f.concurrency.attempts[len(f.concurrency.attempts)-1])
			}
		})
	}
}

func TestClaudeSubscriptionPriority_LoadSnapshotDoesNotHideRealCapacity(t *testing.T) {
	for _, routed := range []bool{false, true} {
		name := "ordinary"
		if routed {
			name = "routed"
		}
		t.Run(name, func(t *testing.T) {
			f := newClaudeSubscriptionFixture(
				claudeSubscriptionAccount(1, AccountTypeOAuth, 1),
				claudeSubscriptionAccount(2, AccountTypeAPIKey, 2),
			)
			f.concurrency.loadMap[1] = &AccountLoadInfo{AccountID: 1, CurrentConcurrency: 1, LoadRate: 100}
			if routed {
				f.route(1)
			}
			result := f.selectAccount(t, "", nil)
			require.Equal(t, int64(1), result.Account.ID, "a load factor or stale snapshot at 100%% does not exhaust ten real concurrency slots")
			require.True(t, result.Acquired)
			require.Contains(t, f.concurrency.attempts, int64(1))
		})
	}
}

func TestClaudeSubscriptionPriority_TriesRemainingAccountsBeforeWaiting(t *testing.T) {
	for _, preference := range []string{"sticky", "model_route"} {
		for _, availableType := range []string{AccountTypeSetupToken, AccountTypeAPIKey} {
			t.Run(preference+"/"+availableType, func(t *testing.T) {
				busy := claudeSubscriptionAccount(1, AccountTypeOAuth, 1)
				busy.Extra = map[string]any{"max_sessions": 1}
				f := newClaudeSubscriptionFixture(busy, claudeSubscriptionAccount(2, availableType, 2))
				sessions := &claudeSubscriptionSessionCache{}
				f.svc.sessionLimitCache = sessions
				f.concurrency.acquireResults[1] = false
				if preference == "sticky" {
					f.sticky.sessionBindings["session"] = 1
				} else {
					f.route(1)
				}
				result := f.selectAccount(t, "session", nil)
				require.Equal(t, int64(2), result.Account.ID, "a busy preferred account must not produce a wait plan before trying available accounts")
				require.True(t, result.Acquired)
				require.Nil(t, result.WaitPlan)
				require.Empty(t, sessions.registered, "skipped wait candidates must not consume session capacity")
			})
		}
	}
}

func TestClaudeSubscriptionPriority_WaitsOnlyAfterBothPoolsAreFull(t *testing.T) {
	busy := claudeSubscriptionAccount(1, AccountTypeOAuth, 10)
	busy.Extra = map[string]any{"max_sessions": 1}
	f := newClaudeSubscriptionFixture(busy, claudeSubscriptionAccount(2, AccountTypeAPIKey, 1))
	f.route(1)
	f.concurrency.acquireResults[1] = false
	f.concurrency.acquireResults[2] = false
	sessions := &claudeSubscriptionSessionCache{}
	f.svc.sessionLimitCache = sessions
	result := f.selectAccount(t, "session", nil)
	require.False(t, result.Acquired)
	require.NotNil(t, result.WaitPlan)
	require.Equal(t, int64(1), result.Account.ID, "waiting should still favor the subscription pool")
	require.Contains(t, f.concurrency.attempts, int64(1))
	require.Contains(t, f.concurrency.attempts, int64(2), "regular slots must be tried before returning a wait plan")
	require.Equal(t, []int64{1}, sessions.registered, "register only the final wait candidate")
}

func TestClaudeSubscriptionPriority_DegradedLoadPaths(t *testing.T) {
	for _, mode := range []string{"batch_disabled", "batch_error", "no_concurrency_service"} {
		for _, busySubscription := range []bool{false, true} {
			if mode == "no_concurrency_service" && busySubscription {
				continue
			}
			name := mode + "/subscription_available"
			if busySubscription {
				name = mode + "/subscription_busy"
			}
			t.Run(name, func(t *testing.T) {
				f := newClaudeSubscriptionFixture(
					claudeSubscriptionAccount(1, AccountTypeOAuth, 100),
					claudeSubscriptionAccount(2, AccountTypeAPIKey, 1),
				)
				f.sticky.sessionBindings["session"] = 2
				f.route(2)
				switch mode {
				case "batch_disabled":
					f.svc.cfg.Gateway.Scheduling.LoadBatchEnabled = false
				case "batch_error":
					f.concurrency.loadBatchErr = errors.New("isolated test load snapshot failure")
				case "no_concurrency_service":
					f.svc.concurrencyService = nil
				}
				f.concurrency.acquireResults[1] = !busySubscription
				result := f.selectAccount(t, "session", nil)
				want := int64(1)
				if busySubscription {
					want = 2
				}
				require.Equal(t, want, result.Account.ID)
				require.True(t, result.Acquired)
				require.Nil(t, result.WaitPlan)
				if busySubscription {
					require.Contains(t, f.concurrency.attempts, int64(1))
				}
			})
		}
	}
}

func TestClaudeSubscriptionPriority_SessionLimitFailureReleasesSlotAndSpills(t *testing.T) {
	subscription := claudeSubscriptionAccount(1, AccountTypeOAuth, 100)
	subscription.Extra = map[string]any{"max_sessions": 1}
	f := newClaudeSubscriptionFixture(subscription, claudeSubscriptionAccount(2, AccountTypeAPIKey, 1))
	f.svc.sessionLimitCache = &claudeSubscriptionSessionCache{denied: map[int64]bool{1: true}}
	result := f.selectAccount(t, "new-session", nil)
	require.Equal(t, int64(2), result.Account.ID)
	require.True(t, result.Acquired)
	require.Contains(t, f.concurrency.released, int64(1), "rejected session must release its acquired subscription slot")
}

func TestClaudeSubscriptionPriority_EligibilityAndMixedPlatform(t *testing.T) {
	t.Run("excluded_subscription", func(t *testing.T) {
		f := newClaudeSubscriptionFixture(claudeSubscriptionAccount(1, AccountTypeOAuth, 100), claudeSubscriptionAccount(2, AccountTypeAPIKey, 1))
		result := f.selectAccount(t, "", map[int64]struct{}{1: {}})
		require.Equal(t, int64(2), result.Account.ID)
		require.NotContains(t, f.concurrency.attempts, int64(1))
	})
	t.Run("subscription_model_not_supported", func(t *testing.T) {
		subscription := claudeSubscriptionAccount(1, AccountTypeOAuth, 100)
		subscription.Credentials = map[string]any{"model_mapping": map[string]any{"different-model": "different-model"}}
		f := newClaudeSubscriptionFixture(subscription, claudeSubscriptionAccount(2, AccountTypeAPIKey, 1))
		result := f.selectAccount(t, "", nil)
		require.Equal(t, int64(2), result.Account.ID)
		require.NotContains(t, f.concurrency.attempts, int64(1))
	})
	t.Run("antigravity_oauth_is_regular_pool", func(t *testing.T) {
		mixed := claudeSubscriptionAccount(2, AccountTypeOAuth, 1)
		mixed.Platform = PlatformAntigravity
		mixed.Extra = map[string]any{"mixed_scheduling": true}
		f := newClaudeSubscriptionFixture(claudeSubscriptionAccount(1, AccountTypeSetupToken, 100), mixed)
		f.sticky.sessionBindings["session"] = 2
		result := f.selectAccount(t, "session", nil)
		require.Equal(t, int64(1), result.Account.ID)
	})
}

func TestClaudeSubscriptionPriority_NonAnthropicKeepsStickyBehavior(t *testing.T) {
	a := claudeSubscriptionAccount(1, AccountTypeOAuth, 1)
	b := claudeSubscriptionAccount(2, AccountTypeAPIKey, 100)
	a.Platform, b.Platform = PlatformGemini, PlatformGemini
	f := newClaudeSubscriptionFixture(a, b)
	f.group.Platform = PlatformGemini
	f.sticky.sessionBindings["session"] = 2
	result, err := f.svc.SelectAccountWithLoadAwareness(context.Background(), &f.group.ID, "session", "gemini-2.5-pro", nil, "", 0)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int64(2), result.Account.ID)
	require.True(t, result.Acquired)
	if result.ReleaseFunc != nil {
		result.ReleaseFunc()
	}
}

func TestClaudeSubscriptionPriority_SelectionOnlyDoesNotAcquireOrRegister(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		name := "subscription_available"
		if excluded {
			name = "subscription_excluded"
		}
		t.Run(name, func(t *testing.T) {
			subscription := claudeSubscriptionAccount(1, AccountTypeSetupToken, 100)
			subscription.Extra = map[string]any{"max_sessions": 1}
			f := newClaudeSubscriptionFixture(subscription, claudeSubscriptionAccount(2, AccountTypeAPIKey, 1))
			f.sticky.sessionBindings["count-tokens"] = 2
			f.route(2)
			f.concurrency.acquireResults[1] = false
			f.concurrency.acquireResults[2] = false
			sessions := &claudeSubscriptionSessionCache{}
			f.svc.sessionLimitCache = sessions
			var excludedIDs map[int64]struct{}
			want := int64(1)
			if excluded {
				excludedIDs = map[int64]struct{}{1: {}}
				want = 2
			}
			account, err := f.svc.SelectAccountForModelWithExclusions(context.Background(), &f.group.ID, "count-tokens", claudeSubscriptionModel, excludedIDs)
			require.NoError(t, err)
			require.NotNil(t, account)
			require.Equal(t, want, account.ID)
			require.Empty(t, f.concurrency.attempts, "count_tokens selection must retain its no-slot lifecycle")
			require.Empty(t, sessions.registered, "selection-only requests must not consume active-session capacity")
		})
	}
}

func TestClaudeSubscriptionPriority_UsesResolvedTargetPlatform(t *testing.T) {
	for _, scope := range []string{"forced_anthropic", "composite_anthropic", "composite_gemini"} {
		for _, selectionOnly := range []bool{false, true} {
			name := scope + "/load_aware"
			if selectionOnly {
				name = scope + "/selection_only"
			}
			t.Run(name, func(t *testing.T) {
				subscription := claudeSubscriptionAccount(1, AccountTypeOAuth, 100)
				regular := claudeSubscriptionAccount(2, AccountTypeAPIKey, 1)
				targetPlatform, requestedModel, want := PlatformAnthropic, claudeSubscriptionModel, int64(1)
				if scope == "composite_gemini" {
					targetPlatform, requestedModel, want = PlatformGemini, "gemini-2.5-pro", 2
					subscription.Platform, regular.Platform = PlatformGemini, PlatformGemini
				}
				f := newClaudeSubscriptionFixture(subscription, regular)
				f.sticky.sessionBindings["session"] = 2
				ctx := context.Background()
				if scope == "forced_anthropic" {
					ctx = context.WithValue(ctx, ctxkey.ForcePlatform, PlatformAnthropic)
				} else {
					f.group.Platform = PlatformComposite
					ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{
						Matched: true, Source: CompositeRouteSourceExplicit, GroupID: f.group.ID,
						PublicModel: requestedModel, TargetPlatform: targetPlatform, UpstreamModel: requestedModel,
						Endpoint: CompositeRouteEndpointAny,
					})
				}
				if selectionOnly {
					account, err := f.svc.SelectAccountForModelWithExclusions(ctx, &f.group.ID, "session", requestedModel, nil)
					require.NoError(t, err)
					require.NotNil(t, account)
					require.Equal(t, want, account.ID)
					require.Empty(t, f.concurrency.attempts)
				} else {
					result, err := f.svc.SelectAccountWithLoadAwareness(ctx, &f.group.ID, "session", requestedModel, nil, "", 0)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, want, result.Account.ID)
					require.True(t, result.Acquired)
					if result.ReleaseFunc != nil {
						result.ReleaseFunc()
					}
				}
			})
		}
	}
}

func TestClaudeSubscriptionPriority_ProfitGatePreservesExistingBinding(t *testing.T) {
	for _, subscriptionEligible := range []bool{true, false} {
		for _, selectionOnly := range []bool{false, true} {
			name := "eligible_subscription/load_aware"
			if !subscriptionEligible {
				name = "ineligible_subscription/load_aware"
			}
			if selectionOnly {
				name += "/selection_only"
			}
			t.Run(name, func(t *testing.T) {
				group := gatewayProfitTestGroup(42, PlatformAnthropic)
				subscription := gatewayProfitTestAccount(1, PlatformAnthropic, 0.2, group.ID)
				subscription.Type = AccountTypeOAuth
				regular := gatewayProfitTestAccount(2, PlatformAnthropic, 0.2, group.ID)
				want, originalBinding := int64(1), int64(2)
				if !subscriptionEligible {
					expensiveRate := 0.8
					subscription.RateMultiplier = &expensiveRate
					want, originalBinding = 2, 1
				}
				f := newClaudeSubscriptionFixture(subscription, regular)
				*f.group = *group
				f.sticky.sessionBindings["profit-session"] = originalBinding
				ctx := gatewayProfitTestContext(f.group)
				if selectionOnly {
					account, err := f.svc.SelectAccountForModelWithExclusions(ctx, &f.group.ID, "profit-session", claudeSubscriptionModel, nil)
					require.NoError(t, err)
					require.NotNil(t, account)
					require.Equal(t, want, account.ID)
				} else {
					result, err := f.svc.SelectAccountWithLoadAwareness(ctx, &f.group.ID, "profit-session", claudeSubscriptionModel, nil, "", 0)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, want, result.Account.ID)
					if result.ReleaseFunc != nil {
						result.ReleaseFunc()
					}
					if !subscriptionEligible {
						require.NotContains(t, f.concurrency.attempts, subscription.ID)
					}
				}
				require.Equal(t, originalBinding, f.sticky.sessionBindings["profit-session"], "pool selection must not overwrite a binding before terminal profit admission")
				require.Zero(t, f.sticky.deletedSessions["profit-session"])
			})
		}
	}
}

func TestClaudeSubscriptionPriority_SlotErrorDoesNotProvePoolExhausted(t *testing.T) {
	for _, mode := range []string{"ordinary", "sticky", "model_route", "batch_disabled", "batch_error"} {
		t.Run(mode, func(t *testing.T) {
			f := newClaudeSubscriptionFixture(
				claudeSubscriptionAccount(1, AccountTypeOAuth, 1),
				claudeSubscriptionAccount(2, AccountTypeAPIKey, 2),
			)
			slotErr := errors.New("isolated test concurrency backend unavailable")
			f.concurrency.acquireErr = map[int64]error{1: slotErr}
			switch mode {
			case "sticky":
				f.sticky.sessionBindings["session"] = 1
			case "model_route":
				f.route(1)
			case "batch_disabled":
				f.svc.cfg.Gateway.Scheduling.LoadBatchEnabled = false
			case "batch_error":
				f.concurrency.loadBatchErr = errors.New("isolated test load snapshot unavailable")
			}
			result, err := f.svc.SelectAccountWithLoadAwareness(context.Background(), &f.group.ID, "session", claudeSubscriptionModel, nil, "", 0)
			require.ErrorIs(t, err, slotErr, "a failed slot operation cannot establish that the subscription pool is full")
			require.Nil(t, result)
			require.NotContains(t, f.concurrency.attempts, int64(2), "concurrency infrastructure errors must not spill traffic to the regular pool")
		})
	}
}

func TestClaudeSubscriptionPriority_CompositeAliasResolvesBeforeDegradedSelection(t *testing.T) {
	for _, source := range []string{"explicit_upstream_rewrite", "account_owned_alias"} {
		for _, mode := range []string{"batch_disabled", "no_concurrency_service"} {
			t.Run(source+"/"+mode, func(t *testing.T) {
				const alias = "private-assistant"
				const upstreamModel = "claude-sonnet-4-5-20250929"
				unrelated := claudeSubscriptionAccount(1, AccountTypeOAuth, 1)
				owner := claudeSubscriptionAccount(2, AccountTypeSetupToken, 2)
				if source == "explicit_upstream_rewrite" {
					unrelated.Credentials = map[string]any{"model_mapping": map[string]any{"different-model": "different-model"}}
					owner.Credentials = map[string]any{"model_mapping": map[string]any{upstreamModel: upstreamModel}}
				} else {
					// Empty mapping permits ordinary models, but must not claim an account-owned alias.
					owner.Credentials = map[string]any{"model_mapping": map[string]any{alias: upstreamModel}}
				}
				f := newClaudeSubscriptionFixture(unrelated, owner, claudeSubscriptionAccount(3, AccountTypeAPIKey, 3))
				f.group.Platform = PlatformComposite
				if source == "explicit_upstream_rewrite" {
					f.svc.compositeResolver = NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{{
						ID: 1, GroupID: f.group.ID, PublicModel: alias, MatchType: CompositeRouteMatchExact,
						TargetPlatform: PlatformAnthropic, UpstreamModel: upstreamModel,
						Endpoint: CompositeRouteEndpointAny, Enabled: true,
					}}})
				} else {
					f.svc.compositeResolver = NewCompositeRouteResolver(nil)
					f.svc.compositeResolver.SetModelOwnershipResolver(func(_ context.Context, _ int64, model string) (CompositeModelOwnership, error) {
						return CompositeModelOwnership{Matched: model == alias, TargetPlatform: PlatformAnthropic}, nil
					})
				}
				if mode == "batch_disabled" {
					f.svc.cfg.Gateway.Scheduling.LoadBatchEnabled = false
				} else {
					f.svc.concurrencyService = nil
				}
				// Deliberately provide no pre-resolved Composite context, as the legacy selector did its own resolution.
				result, err := f.svc.SelectAccountWithLoadAwareness(context.Background(), &f.group.ID, "", alias, nil, "", 0)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, owner.ID, result.Account.ID, "the resolved upstream model and alias ownership must survive the subscription pool branch")
				require.True(t, result.Acquired)
				if result.ReleaseFunc != nil {
					result.ReleaseFunc()
				}
			})
		}
	}
}

func TestClaudeSubscriptionPriority_HydrationFailureReleasesAcquiredSlot(t *testing.T) {
	subscription := claudeSubscriptionAccount(1, AccountTypeOAuth, 1)
	regular := claudeSubscriptionAccount(2, AccountTypeAPIKey, 2)
	f := newClaudeSubscriptionFixture(subscription, regular)
	snapshot := &snapshotHydrationCache{
		snapshot: []*Account{&subscription, &regular},
		accounts: map[int64]*Account{},
	}
	f.svc.schedulerSnapshot = NewSchedulerSnapshotService(snapshot, nil, stubOpenAIAccountRepo{}, f.svc.groupRepo, nil)
	result, err := f.svc.SelectAccountWithLoadAwareness(context.Background(), &f.group.ID, "", claudeSubscriptionModel, nil, "", 0)
	require.Error(t, err, "the selected account disappeared before credentials could be hydrated")
	require.Nil(t, result)
	require.Equal(t, []int64{subscription.ID}, f.concurrency.released, "hydration failure must release its reserved slot exactly once")
	require.NotContains(t, f.concurrency.attempts, regular.ID)
}

func (f *claudeSubscriptionFixture) enableGroups(ids ...int64) {
	raw, _ := json.Marshal(ids)
	f.svc.settingService = NewSettingService(&betaPolicySettingRepoStub{values: map[string]string{
		SettingKeyClaudeSubscriptionPriorityGroupIDs: string(raw),
	}}, f.svc.cfg)
}

func TestClaudeSubscriptionPriority_OnlyEnabledRequestGroups(t *testing.T) {
	for _, mode := range []string{"load_aware", "batch_disabled", "selection_only"} {
		for _, enabled := range []bool{false, true} {
			name := mode + "/disabled"
			if enabled {
				name = mode + "/enabled"
			}
			t.Run(name, func(t *testing.T) {
				f := newClaudeSubscriptionFixture(claudeSubscriptionAccount(1, AccountTypeOAuth, 100), claudeSubscriptionAccount(2, AccountTypeAPIKey, 1))
				f.group.Name = "name is not the scheduling identity"
				if enabled {
					f.enableGroups(42)
				} else {
					f.enableGroups(28)
				}
				f.sticky.sessionBindings["session"] = 2
				f.route(2)
				want := int64(2)
				if enabled {
					want = 1
				}
				if mode == "selection_only" {
					account, err := f.svc.SelectAccountForModelWithExclusions(context.Background(), &f.group.ID, "session", claudeSubscriptionModel, nil)
					require.NoError(t, err)
					require.Equal(t, want, account.ID)
				} else {
					if mode == "batch_disabled" {
						f.svc.cfg.Gateway.Scheduling.LoadBatchEnabled = false
					}
					result := f.selectAccount(t, "session", nil)
					require.Equal(t, want, result.Account.ID)
				}
			})
		}
	}
}

func TestClaudeSubscriptionPriority_FollowsFallbackGroupScope(t *testing.T) {
	for _, selectionOnly := range []bool{false, true} {
		for _, enableFallback := range []bool{false, true} {
			name := "load_aware/source_enabled"
			if selectionOnly {
				name = "selection_only/source_enabled"
			}
			if enableFallback {
				name += "/fallback_enabled"
			}
			t.Run(name, func(t *testing.T) {
				f := newClaudeSubscriptionFixture(claudeSubscriptionAccount(1, AccountTypeOAuth, 100), claudeSubscriptionAccount(2, AccountTypeAPIKey, 1))
				fallbackID := int64(24)
				f.group.ClaudeCodeOnly = true
				f.group.FallbackGroupID = &fallbackID
				fallback := &Group{ID: fallbackID, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true}
				f.svc.groupRepo = &mockGroupRepoForGateway{groups: map[int64]*Group{f.group.ID: f.group, fallbackID: fallback}}
				for i := range f.repo.accounts {
					f.repo.accounts[i].AccountGroups = append(f.repo.accounts[i].AccountGroups, AccountGroup{GroupID: fallbackID})
				}
				want := int64(2)
				if enableFallback {
					f.enableGroups(fallbackID)
					want = 1
				} else {
					f.enableGroups(f.group.ID)
				}
				if selectionOnly {
					account, err := f.svc.SelectAccountForModelWithExclusions(context.Background(), &f.group.ID, "", claudeSubscriptionModel, nil)
					require.NoError(t, err)
					require.Equal(t, want, account.ID)
				} else {
					result := f.selectAccount(t, "", nil)
					require.Equal(t, want, result.Account.ID)
				}
			})
		}
	}
}

func TestClaudeSubscriptionPriority_DefaultOff(t *testing.T) {
	for _, mode := range []string{"missing_service", "missing_setting", "invalid_setting", "no_group"} {
		t.Run(mode, func(t *testing.T) {
			f := newClaudeSubscriptionFixture(claudeSubscriptionAccount(1, AccountTypeOAuth, 100), claudeSubscriptionAccount(2, AccountTypeAPIKey, 1))
			groupID := &f.group.ID
			switch mode {
			case "missing_service":
				f.svc.settingService = nil
			case "missing_setting":
				f.svc.settingService = NewSettingService(&betaPolicySettingRepoStub{}, f.svc.cfg)
			case "invalid_setting":
				f.svc.settingService = NewSettingService(&betaPolicySettingRepoStub{values: map[string]string{SettingKeyClaudeSubscriptionPriorityGroupIDs: "invalid"}}, f.svc.cfg)
			case "no_group":
				groupID = nil
			}
			account, err := f.svc.SelectAccountForModelWithExclusions(context.Background(), groupID, "", claudeSubscriptionModel, nil)
			require.NoError(t, err)
			require.Equal(t, int64(2), account.ID)
		})
	}
}
