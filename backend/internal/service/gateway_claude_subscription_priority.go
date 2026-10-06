package service

import (
	"context"
	"errors"
	"log/slog"
)

type gatewayAccountPoolRequest struct {
	groupID         *int64
	group           *Group
	sessionHash     string
	requestedModel  string
	excludedIDs     map[int64]struct{}
	platform        string
	useMixed        bool
	stickyAccountID int64
}

// Default-off opt-in follows the final group after any Claude Code fallback.
// Account membership is deliberately not used: the same account may serve groups
// whose scheduling policies differ.
func (s *GatewayService) isClaudeSubscriptionPriorityEnabled(ctx context.Context, groupID *int64) bool {
	return groupID != nil && s.settingService != nil && s.settingService.IsClaudeSubscriptionPriorityEnabled(ctx, *groupID)
}

// Claude subscriptions use Anthropic OAuth or Setup Token credentials. API keys
// and eligible mixed-platform accounts remain available in the fallback pool.
func partitionClaudeSubscriptionAccounts(accounts []Account) ([]Account, []Account) {
	var subscriptions, regular []Account
	for _, account := range accounts {
		if account.IsAnthropicOAuthOrSetupToken() {
			subscriptions = append(subscriptions, account)
		} else {
			regular = append(regular, account)
		}
	}
	return subscriptions, regular
}

func (s *GatewayService) selectClaudeSubscriptionPool(ctx context.Context, request gatewayAccountPoolRequest, accounts []Account) (*AccountSelectionResult, error) {
	subscriptions, regular := partitionClaudeSubscriptionAccounts(accounts)
	if len(subscriptions) == 0 {
		// With no subscription pool there is no overflow decision to make.
		// Preserve ordinary routing, sticky waiting and load filtering exactly.
		if s.concurrencyService == nil || !s.schedulingConfig().LoadBatchEnabled {
			return s.selectGatewayAccountWithoutLoad(ctx, request.groupID, request.sessionHash, request.requestedModel, request.excludedIDs, request.stickyAccountID)
		}
		return s.selectGatewayAccountPool(ctx, request, regular, false, true)
	}
	pools := [][]Account{subscriptions, regular}
	lastErr := ErrNoAvailableAccounts
	// First exhaust immediate admission in both pools. Only then may a wait plan
	// register a session. Speculative wait plans cannot safely be undone: another
	// in-flight request may already own the same account/session registration.
	for _, allowWait := range []bool{false, true} {
		for i, pool := range pools {
			if len(pool) == 0 {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			selection, err := s.selectGatewayAccountPool(ctx, request, pool, true, allowWait)
			if err != nil {
				if !errors.Is(err, ErrNoAvailableAccounts) {
					return nil, err
				}
				lastErr = err
				continue
			}
			if selection != nil && selection.Account != nil {
				poolName := "subscription"
				if i == 1 {
					poolName = "regular"
				}
				slog.Info("claude.subscription_pool_selected",
					"group_id", derefGroupID(request.groupID),
					"pool", poolName, "account_id", selection.Account.ID,
					"acquired", selection.Acquired)
				return selection, nil
			}
		}
	}
	return nil, lastErr
}

type claudeAccountSelectionPool struct {
	subscription bool
	group        *Group
}

func matchesClaudeSubscriptionPool(account *Account, pool *claudeAccountSelectionPool) bool {
	return pool == nil || (account != nil && account.IsAnthropicOAuthOrSetupToken() == pool.subscription)
}

// Token counting and other selection-only callers use the same pool order but
// retain their existing no-concurrency-slot lifecycle.
func (s *GatewayService) selectClaudeAccountForModel(ctx context.Context, groupID *int64, sessionHash, requestedModel string, excludedIDs map[int64]struct{}, hasForcePlatform bool) (*Account, error) {
	// Read privacy requirements once for both passes, including a failed lookup.
	var group *Group
	if groupID != nil && s.groupRepo != nil {
		group, _ = s.groupRepo.GetByIDLite(ctx, *groupID)
	}
	lastErr := ErrNoAvailableAccounts
	for _, subscription := range []bool{true, false} {
		pool := &claudeAccountSelectionPool{subscription: subscription, group: group}
		var account *Account
		var err error
		if hasForcePlatform {
			account, err = s.selectAccountForModelWithPlatformPool(ctx, groupID, sessionHash, requestedModel, excludedIDs, PlatformAnthropic, pool)
		} else {
			account, err = s.selectAccountWithMixedSchedulingPool(ctx, groupID, sessionHash, requestedModel, excludedIDs, PlatformAnthropic, pool)
		}
		if err == nil {
			return s.hydrateSelectedAccount(ctx, account)
		}
		if !errors.Is(err, ErrNoAvailableAccounts) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}
