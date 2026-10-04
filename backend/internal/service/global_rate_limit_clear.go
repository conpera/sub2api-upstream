package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// GlobalRateLimitClearExpectation is the exact account generation verified by
// the caller. It does not authorize clearing model, overload or auth failures.
type GlobalRateLimitClearExpectation struct {
	UpdatedAt   time.Time `json:"expected_updated_at"`
	LimitedAt   time.Time `json:"expected_rate_limited_at"`
	ResetAt     time.Time `json:"expected_rate_limit_reset_at"`
	TokenSHA256 string    `json:"expected_token_sha256"`
	Status      string    `json:"expected_status"`
	Schedulable *bool     `json:"expected_schedulable"`
	ProxyID     *int64    `json:"expected_proxy_id"`
	GroupIDs    []int64   `json:"expected_group_ids"`
}

func (e GlobalRateLimitClearExpectation) Valid() bool {
	hash, err := hex.DecodeString(e.TokenSHA256)
	return err == nil && len(hash) == sha256.Size && !e.UpdatedAt.IsZero() && !e.LimitedAt.IsZero() && !e.ResetAt.IsZero() &&
		e.Schedulable != nil && e.ProxyID != nil && *e.ProxyID >= 0 && e.GroupIDs != nil &&
		(e.Status == StatusActive || e.Status == "inactive")
}

type globalRateLimitClearRepository interface {
	ClearGlobalRateLimitIfUnchanged(context.Context, *Account) (bool, error)
}

func (s *RateLimitService) ClearGlobalRateLimitIfUnchanged(ctx context.Context, accountID int64, expected GlobalRateLimitClearExpectation) (bool, error) {
	if !expected.Valid() {
		return false, errors.New("invalid global rate limit expectation")
	}
	repo, ok := s.accountRepo.(globalRateLimitClearRepository)
	if !ok {
		return false, errors.New("conditional global rate limit clear unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return false, err
	}
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.IsShadow() ||
		!account.UpdatedAt.Equal(expected.UpdatedAt) || account.RateLimitedAt == nil || !account.RateLimitedAt.Equal(expected.LimitedAt) ||
		account.RateLimitResetAt == nil || !account.RateLimitResetAt.Equal(expected.ResetAt) ||
		account.Status != expected.Status || account.Schedulable != *expected.Schedulable {
		return false, nil
	}
	proxyID := int64(0)
	if account.ProxyID != nil {
		proxyID = *account.ProxyID
	}
	groups, expectedGroups := slices.Clone(account.GroupIDs), slices.Clone(expected.GroupIDs)
	slices.Sort(groups)
	slices.Sort(expectedGroups)
	if proxyID != *expected.ProxyID || !slices.Equal(groups, expectedGroups) {
		return false, nil
	}
	tokens, err := json.Marshal([]string{account.GetCredential("access_token"), account.GetCredential("refresh_token"), account.GetCredential("id_token")})
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256(tokens)
	if hex.EncodeToString(hash[:]) != expected.TokenSHA256 {
		return false, nil
	}
	// The repository compares this full snapshot inside the UPDATE. Do not call
	// ClearRateLimit: that action also resets unrelated runtime restrictions.
	return repo.ClearGlobalRateLimitIfUnchanged(ctx, account)
}
