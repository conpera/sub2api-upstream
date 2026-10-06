package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type claudePriorityHandlerRepo struct {
	service.SettingRepository
	values      map[string]string
	readErr     error
	writtenKeys []string
}

func (r *claudePriorityHandlerRepo) GetValue(_ context.Context, key string) (string, error) {
	if r.readErr != nil {
		return "", r.readErr
	}
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *claudePriorityHandlerRepo) Set(_ context.Context, key, value string) error {
	r.writtenKeys = append(r.writtenKeys, key)
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

type claudePriorityHandlerGroupReader struct{}

func (claudePriorityHandlerGroupReader) GetByID(_ context.Context, id int64) (*service.Group, error) {
	if id == 28 {
		return &service.Group{ID: id, Platform: service.PlatformAnthropic, Status: service.StatusActive}, nil
	}
	return nil, service.ErrGroupNotFound
}

func claudePriorityHandlerRequest(t *testing.T, h *SettingHandler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, "/api/v1/admin/settings/claude-subscription-priority", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if method == http.MethodGet {
		h.GetClaudeSubscriptionPriority(c)
	} else {
		h.UpdateClaudeSubscriptionPriority(c)
	}
	return rec
}

func TestSettingHandler_ClaudeSubscriptionPriorityRequiresExplicitArray(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{`{}`, `{"enabled_group_ids":null}`, `{"enabled_group_ids":"28"}`, `{"enabled_group_ids":[1.5]}`, `{"enabled_group_ids":["28"]}`, `null`, `[]`, `{"enabled_group_ids":[0]}`, `{"enabled_group_ids":[99]}`} {
		t.Run(body, func(t *testing.T) {
			repo := &claudePriorityHandlerRepo{values: map[string]string{service.SettingKeyClaudeSubscriptionPriorityGroupIDs: "[28]"}}
			svc := service.NewSettingService(repo, &config.Config{})
			svc.SetDefaultSubscriptionGroupReader(claudePriorityHandlerGroupReader{})
			handler := NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
			rec := claudePriorityHandlerRequest(t, handler, http.MethodPut, body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Empty(t, repo.writtenKeys)
			require.Equal(t, "[28]", repo.values[service.SettingKeyClaudeSubscriptionPriorityGroupIDs])
		})
	}
}

func TestSettingHandler_ClaudeSubscriptionPriorityRoundTripAndClear(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &claudePriorityHandlerRepo{values: map[string]string{"unrelated": "keep"}}
	svc := service.NewSettingService(repo, &config.Config{})
	svc.SetDefaultSubscriptionGroupReader(claudePriorityHandlerGroupReader{})
	handler := NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
	initial := claudePriorityHandlerRequest(t, handler, http.MethodGet, "")
	require.Equal(t, http.StatusOK, initial.Code)
	for _, tc := range []struct {
		body, stored string
		want         []int64
	}{
		{`{"enabled_group_ids":[28,28]}`, "[28]", []int64{28}},
		{`{"enabled_group_ids":[]}`, "[]", []int64{}},
	} {
		rec := claudePriorityHandlerRequest(t, handler, http.MethodPut, tc.body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		get := claudePriorityHandlerRequest(t, handler, http.MethodGet, "")
		require.Equal(t, http.StatusOK, get.Code, get.Body.String())
		var payload struct {
			Data service.ClaudeSubscriptionPrioritySettings `json:"data"`
		}
		require.NoError(t, json.Unmarshal(get.Body.Bytes(), &payload))
		require.Equal(t, tc.want, payload.Data.EnabledGroupIDs)
		require.True(t, payload.Data.Supported)
		require.Equal(t, "subscription_first", payload.Data.Strategy)
		require.Equal(t, []string{"oauth", "setup-token"}, payload.Data.SubscriptionAccountTypes)
		require.Equal(t, tc.stored, repo.values[service.SettingKeyClaudeSubscriptionPriorityGroupIDs])
		require.Equal(t, len(tc.want) > 0, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
	}
	require.Equal(t, "keep", repo.values["unrelated"])
	require.Equal(t, []string{service.SettingKeyClaudeSubscriptionPriorityGroupIDs, service.SettingKeyClaudeSubscriptionPriorityGroupIDs}, repo.writtenKeys)
}

func TestSettingHandler_ClaudeSubscriptionPriorityReadFailureIsNotDisabledResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &claudePriorityHandlerRepo{readErr: errors.New("isolated database outage")}
	svc := service.NewSettingService(repo, &config.Config{})
	handler := NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
	rec := claudePriorityHandlerRequest(t, handler, http.MethodGet, "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "enabled_group_ids")
	require.False(t, svc.IsClaudeSubscriptionPriorityEnabled(context.Background(), 28))
}
