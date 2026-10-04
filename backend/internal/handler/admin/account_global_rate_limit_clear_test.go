//go:build unit

package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type globalClearHandlerRepo struct {
	service.AccountRepository
	account *service.Account
	matched bool
	calls   int
}

func (r *globalClearHandlerRepo) GetByID(context.Context, int64) (*service.Account, error) {
	return r.account, nil
}
func (r *globalClearHandlerRepo) ClearGlobalRateLimitIfUnchanged(context.Context, *service.Account) (bool, error) {
	r.calls++
	return r.matched, nil
}
func TestConditionalGlobalClearHandlerContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	limited, reset := now.Add(-time.Hour), now.Add(time.Hour)
	sched := true
	proxy := int64(0)
	hash := sha256.Sum256([]byte(`["access","",""]`))
	expected := service.GlobalRateLimitClearExpectation{UpdatedAt: now, LimitedAt: limited, ResetAt: reset, TokenSHA256: hex.EncodeToString(hash[:]), Status: service.StatusActive, Schedulable: &sched, ProxyID: &proxy, GroupIDs: []int64{}}
	repo := &globalClearHandlerRepo{matched: true, account: &service.Account{ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, UpdatedAt: now, RateLimitedAt: &limited, RateLimitResetAt: &reset, Credentials: map[string]any{"access_token": "access"}}}
	h := &AccountHandler{rateLimitService: service.NewRateLimitService(repo, nil, nil, nil, nil)}
	router := gin.New()
	router.POST("/accounts/:id/clear-global-rate-limit-if-unchanged", h.ClearGlobalRateLimitIfUnchanged)
	send := func(body []byte) int {
		req := httptest.NewRequest(http.MethodPost, "/accounts/42/clear-global-rate-limit-if-unchanged", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	require.Equal(t, 400, send([]byte(`{}`)))
	require.Zero(t, repo.calls)
	body, err := json.Marshal(expected)
	require.NoError(t, err)
	require.Equal(t, 200, send(body))
	require.Equal(t, 1, repo.calls)
	repo.matched = false
	require.Equal(t, 409, send(body))
	require.Equal(t, 2, repo.calls)
	repo.account.Credentials["access_token"] = "rotated"
	require.Equal(t, 409, send(body))
	require.Equal(t, 2, repo.calls)
}
