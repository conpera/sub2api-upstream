//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func globalClearAccount(t *testing.T, client *dbent.Client) *service.Account {
	limited, reset := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	return mustCreateAccount(t, client, &service.Account{Name: "global-clear", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, RateLimitedAt: &limited, RateLimitResetAt: &reset, Credentials: map[string]any{"access_token": "old", "refresh_token": "old-rt"}})
}
func TestClearGlobalRateLimitPreservesUnrelatedRestrictions(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	a := globalClearAccount(t, tx.Client())
	until := time.Now().Add(2 * time.Hour)
	_, err := tx.Client().Account.UpdateOneID(a.ID).SetOverloadUntil(until).SetTempUnschedulableUntil(until).SetTempUnschedulableReason("keep").SetErrorMessage("keep error").SetExtra(map[string]any{"model_rate_limits": map[string]any{"model": "keep"}, "auto_reset_credit_enabled": true}).Save(ctx)
	require.NoError(t, err)
	before, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	cleared, err := repo.ClearGlobalRateLimitIfUnchanged(ctx, before)
	require.NoError(t, err)
	require.True(t, cleared)
	after, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, after.RateLimitedAt)
	require.Nil(t, after.RateLimitResetAt)
	before.RateLimitedAt = nil
	before.RateLimitResetAt = nil
	before.UpdatedAt = after.UpdatedAt
	require.Equal(t, before, after, "only the two global markers and row version may change")
	var count int
	require.NoError(t, scanSingleRow(ctx, tx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=$1", []any{a.ID}, &count))
	require.Equal(t, 1, count)
	cleared, err = repo.ClearGlobalRateLimitIfUnchanged(ctx, before)
	require.NoError(t, err)
	require.False(t, cleared)
}
func TestClearGlobalRateLimitRejectsConcurrentChanges(t *testing.T) {
	for name, sql := range map[string]string{
		"version":          "updated_at=updated_at+interval '1 second'",
		"same reset rearm": "rate_limited_at=rate_limited_at+interval '1 second'",
		"reset":            "rate_limit_reset_at=rate_limit_reset_at+interval '1 second'",
		"credentials":      "credentials=credentials || '{\"access_token\":\"new\"}'::jsonb",
		"status":           "status='inactive'", "scheduling": "schedulable=false",
		"model":     "extra='{\"model_rate_limits\":{\"new\":true}}'::jsonb",
		"temporary": "temp_unschedulable_until=NOW()+interval '1 hour'",
		"overload":  "overload_until=NOW()+interval '1 hour'",
		"expiry":    "expires_at=NOW()", "deleted": "deleted_at=NOW()",
		"platform": "platform='anthropic'",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			a := globalClearAccount(t, tx.Client())
			observed, err := repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, "UPDATE accounts SET "+sql+" WHERE id=$1", a.ID)
			require.NoError(t, err)
			cleared, err := repo.ClearGlobalRateLimitIfUnchanged(ctx, observed)
			require.NoError(t, err)
			require.False(t, cleared)
			var count int
			require.NoError(t, scanSingleRow(ctx, tx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=$1", []any{a.ID}, &count))
			require.Zero(t, count)
		})
	}
}
func TestClearGlobalRateLimitOutboxFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	a := globalClearAccount(t, client)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id=$1", a.ID)
		_ = client.Account.DeleteOneID(a.ID).Exec(ctx)
	})
	repo := newAccountRepositoryWithSQL(client, &failAtomicSchedulerOutboxSQLExecutor{sqlExecutor: integrationDB}, nil)
	before, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	cleared, err := repo.ClearGlobalRateLimitIfUnchanged(ctx, before)
	require.Error(t, err)
	require.False(t, cleared)
	after, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestClearGlobalRateLimitWaitsForConcurrentGroupMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := testEntClient(t)
	a := globalClearAccount(t, client)
	group := mustCreateGroup(t, client, &service.Group{Name: "global-clear-concurrent-group", Platform: service.PlatformOpenAI})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id=$1", a.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM account_groups WHERE account_id=$1", a.ID)
		_ = client.Account.DeleteOneID(a.ID).Exec(context.Background())
		_ = client.Group.DeleteOneID(group.ID).Exec(context.Background())
	})
	reader := newAccountRepositoryWithSQL(client, integrationDB, nil)
	observed, err := reader.GetByID(ctx, a.ID)
	require.NoError(t, err)
	groupTx, err := client.Tx(ctx)
	require.NoError(t, err)
	defer func() { _ = groupTx.Rollback() }()
	groupRepo := newAccountRepositoryWithSQL(groupTx.Client(), groupTx, nil)
	require.NoError(t, groupRepo.BindGroups(ctx, a.ID, []int64{group.ID}))
	conn, err := integrationDB.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	var pid int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid))
	clearRepo := newAccountRepositoryWithSQL(client, conn, nil)
	type result struct {
		cleared bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		cleared, err := clearRepo.ClearGlobalRateLimitIfUnchanged(ctx, observed)
		done <- result{cleared, err}
	}()
	require.Eventually(t, func() bool {
		var wait string
		err := integrationDB.QueryRowContext(ctx, "SELECT COALESCE(wait_event_type,'') FROM pg_stat_activity WHERE pid=$1", pid).Scan(&wait)
		return err == nil && wait == "Lock"
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, groupTx.Commit())
	got := <-done
	require.NoError(t, got.err)
	require.False(t, got.cleared)
	after, err := reader.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{group.ID}, after.GroupIDs)
	require.NotNil(t, after.RateLimitResetAt)
}
