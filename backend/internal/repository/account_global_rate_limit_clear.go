package repository

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ClearGlobalRateLimitIfUnchanged clears only the observed global generation.
// The outbox insert is atomic with the mutation; a failure rolls both back.
func (r *accountRepository) ClearGlobalRateLimitIfUnchanged(ctx context.Context, observed *service.Account) (bool, error) {
	credentials, err := json.Marshal(normalizeJSONMap(observed.Credentials))
	if err != nil {
		return false, err
	}
	extra, err := json.Marshal(normalizeJSONMap(observed.Extra))
	if err != nil {
		return false, err
	}
	groups := append([]int64{}, observed.GroupIDs...)
	slices.Sort(groups)
	groupJSON, err := json.Marshal(groups)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
 WITH updated AS (
  UPDATE accounts AS a SET rate_limited_at = NULL, rate_limit_reset_at = NULL, updated_at = NOW()
  WHERE a.id = $1 AND a.deleted_at IS NULL AND a.platform = $2 AND a.type = $3
   AND a.updated_at = $4 AND a.rate_limited_at = $5 AND a.rate_limit_reset_at = $6
   AND a.credentials = $7::jsonb AND COALESCE(a.extra, '{}'::jsonb) = $8::jsonb
   AND a.status = $9 AND a.schedulable = $10 AND a.proxy_id IS NOT DISTINCT FROM $11
   AND a.expires_at IS NOT DISTINCT FROM $12 AND a.auto_pause_on_expired = $13
   AND a.overload_until IS NOT DISTINCT FROM $14 AND a.temp_unschedulable_until IS NOT DISTINCT FROM $15
   AND COALESCE(a.temp_unschedulable_reason, '') = $16 AND COALESCE(a.error_message, '') = $17
   AND COALESCE((SELECT jsonb_agg(ag.group_id ORDER BY ag.group_id) FROM account_groups ag WHERE ag.account_id = a.id), '[]'::jsonb) = $18::jsonb
  RETURNING a.id
 )
 INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
 SELECT $19, updated.id, NULL, NULL FROM updated
 `, observed.ID, service.PlatformOpenAI, service.AccountTypeOAuth, observed.UpdatedAt, observed.RateLimitedAt, observed.RateLimitResetAt,
		string(credentials), string(extra), observed.Status, observed.Schedulable, observed.ProxyID,
		observed.ExpiresAt, observed.AutoPauseOnExpired, observed.OverloadUntil, observed.TempUnschedulableUntil,
		observed.TempUnschedulableReason, observed.ErrorMessage, string(groupJSON), service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, observed.ID)
	return true, nil
}
