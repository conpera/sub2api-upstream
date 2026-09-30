package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIGatewayService_SubscriptionPriorityAPIKeyOptIn(t *testing.T) {
	const subscriptionID int64 = 21631
	const optedInID int64 = 21632
	const regularID int64 = 21633
	cases := []struct {
		name                string
		flag                any
		globalDisabled      bool
		withoutSubscription bool
		topK                int
		mutate              func(*Account)
		busy                []int64
		want                int64
	}{
		{name: "absent preserves subscription priority", want: subscriptionID},
		{name: "false opts out", flag: false, want: subscriptionID},
		{name: "true competes with available subscription", flag: true, want: optedInID},
		{name: "string true does not opt in", flag: "true", want: subscriptionID},
		{name: "API-only group opt-in gets priority tier", flag: true, withoutSubscription: true, want: optedInID},
		{name: "API-only group without opt-in compares all priorities", withoutSubscription: true, want: regularID},
		{name: "existing top K fallback remains unchanged", flag: true, topK: 1, busy: []int64{optedInID}, want: regularID},
		{name: "global setting off uses all accounts", flag: true, globalDisabled: true, want: regularID},
		{name: "still respects account priority", flag: true, mutate: func(a *Account) { a.Priority = 20 }, want: subscriptionID},
		{name: "full API channel falls back to subscription", flag: true, busy: []int64{optedInID}, want: subscriptionID},
		{name: "full priority pool falls back to regular", flag: true, busy: []int64{optedInID, subscriptionID}, want: regularID},
		{name: "unschedulable API channel is excluded", flag: true, mutate: func(a *Account) { a.Schedulable = false }, want: subscriptionID},
		{name: "error API channel is excluded", flag: true, mutate: func(a *Account) { a.Status = StatusError }, want: subscriptionID},
		{name: "model whitelist still applies", flag: true, mutate: func(a *Account) {
			a.Credentials["model_mapping"] = map[string]any{"gpt-4o": "gpt-4o"}
		}, want: subscriptionID},
		{name: "flag does not promote free OAuth", flag: true, mutate: func(a *Account) {
			a.Type = AccountTypeOAuth
			a.Credentials["plan_type"] = "free"
		}, want: subscriptionID},
		{name: "flag does not promote another platform", flag: true, mutate: func(a *Account) { a.Platform = PlatformAnthropic }, want: subscriptionID},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			groupID := int64(10130)
			apiAccount := Account{
				ID: optedInID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true, Concurrency: 10, Priority: 1,
				GroupIDs:    []int64{groupID},
				Credentials: map[string]any{"plan_type": "self_serve_business_prolite", "api_key": "test-key", "base_url": "https://channel.example/v1"},
			}
			if tt.flag != nil {
				apiAccount.Extra = map[string]any{"openai_subscription_priority_enabled": tt.flag}
			}
			if tt.mutate != nil {
				tt.mutate(&apiAccount)
			}
			accounts := []Account{
				{ID: subscriptionID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
					Schedulable: true, Concurrency: 10, Priority: 10, GroupIDs: []int64{groupID},
					Credentials: map[string]any{"plan_type": "team"}},
				apiAccount,
				{ID: regularID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
					Schedulable: true, Concurrency: 10, Priority: 0, GroupIDs: []int64{groupID}},
			}
			if tt.withoutSubscription {
				accounts = accounts[1:]
			}
			cache := schedulerTestConcurrencyCache{
				acquireResults: map[int64]bool{subscriptionID: true, optedInID: true, regularID: true},
				loadMap: map[int64]*AccountLoadInfo{
					subscriptionID: {AccountID: subscriptionID},
					optedInID:      {AccountID: optedInID},
					regularID:      {AccountID: regularID},
				},
			}
			for _, id := range tt.busy {
				cache.acquireResults[id] = false
			}
			global := "true"
			if tt.globalDisabled {
				global = "false"
			}
			cfg := newSchedulerTestSubscriptionPriorityConfig()
			if len(tt.busy) > 0 {
				// Probe both priority-pool candidates when checking acquisition fallback.
				cfg.Gateway.OpenAIWS.LBTopK = 2
			}
			if tt.topK > 0 {
				cfg.Gateway.OpenAIWS.LBTopK = tt.topK
			}
			svc := &OpenAIGatewayService{
				accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
				cache:              &schedulerTestGatewayCache{},
				cfg:                cfg,
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true", "", global),
				concurrencyService: NewConcurrencyService(cache),
			}
			selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.NotNil(t, selection.Account)
			if selection.ReleaseFunc != nil {
				defer selection.ReleaseFunc()
			}
			require.True(t, selection.Acquired)
			require.Equal(t, tt.want, selection.Account.ID)
		})
	}
}

// Opting into scheduling must not turn an API key into a ChatGPT/OAuth credential.
func TestOpenAISubscriptionPriorityAPIKeyPreservesForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "channel-test-key", "base_url": "https://channel.example/v1", "plan_type": "team"},
		Extra:       map[string]any{"openai_subscription_priority_enabled": true},
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{
		Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}},
	}}
	token, authType, err := svc.GetAccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "channel-test-key", token)
	require.Equal(t, AccountTypeAPIKey, authType)
	require.False(t, account.IsOpenAIChatGPTSubscription())
	require.False(t, account.UsesOpenAICodexProtocol())
	body := []byte(`{"model":"gpt-5.1","input":"hello","stream":true}`)
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough], func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			var req *http.Request
			var err error
			if passthrough {
				req, err = svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, token)
			} else {
				req, err = svc.buildUpstreamRequest(context.Background(), c, account, body, token, true, "", false)
			}
			require.NoError(t, err)
			require.Equal(t, "https://channel.example/v1/responses", req.URL.String())
			require.Equal(t, "Bearer channel-test-key", req.Header.Get("Authorization"))
			require.Empty(t, req.Header.Get("Chatgpt-Account-Id"))
			forwarded, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.JSONEq(t, string(body), string(forwarded))
		})
	}
}
