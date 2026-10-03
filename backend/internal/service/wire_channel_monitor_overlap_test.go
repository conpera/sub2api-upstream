//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type overlapMonitorRepo struct {
	ChannelMonitorRepository
	monitor *ChannelMonitor
	history chan []*ChannelMonitorHistoryRow
}

func (r *overlapMonitorRepo) GetByID(context.Context, int64) (*ChannelMonitor, error) {
	clone := *r.monitor
	return &clone, nil
}

func (r *overlapMonitorRepo) ListEnabled(ctx context.Context) ([]*ChannelMonitor, error) {
	m, err := r.GetByID(ctx, r.monitor.ID)
	return []*ChannelMonitor{m}, err
}

func (r *overlapMonitorRepo) Update(_ context.Context, monitor *ChannelMonitor) error {
	clone := *monitor
	r.monitor = &clone
	return nil
}

func (r *overlapMonitorRepo) InsertHistoryBatch(_ context.Context, rows []*ChannelMonitorHistoryRow) error {
	r.history <- rows
	return nil
}

func (r *overlapMonitorRepo) MarkChecked(context.Context, int64, time.Time) error {
	return nil
}

type overlapMonitorSettings struct {
	SettingRepository
}

func (r *overlapMonitorSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{
		SettingKeyChannelMonitorEnabled: "true",
		SettingKeyChannelMonitorMode:    ChannelMonitorModeV1,
	}, nil
}

func TestProvideChannelMonitorRunner_ProcessActiveProbes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       *config.Config
		automatic bool
	}{
		{name: "enabled", cfg: &config.Config{ChannelMonitor: config.ChannelMonitorConfig{ActiveProbesEnabled: true}}, automatic: true},
		{name: "nil config keeps existing startup", automatic: true},
		{name: "disabled candidate", cfg: &config.Config{ChannelMonitor: config.ChannelMonitorConfig{ActiveProbesEnabled: false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapMonitorHTTPClient(t)
			var probes atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusOK)
					return
				}
				probes.Add(1)
				defer r.Body.Close()
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{"message": map[string]any{"content": answerFromOpenAIRequest(body)}}},
				})
			}))
			t.Cleanup(upstream.Close)
			repo := &overlapMonitorRepo{
				monitor: &ChannelMonitor{
					ID: 1, Name: "local probe", Enabled: true, IntervalSeconds: 60,
					Provider: MonitorProviderOpenAI, APIMode: MonitorAPIModeChatCompletions,
					Endpoint: upstream.URL, PrimaryModel: "local-model", APIKey: "OLD:local-test-key",
				},
				history: make(chan []*ChannelMonitorHistoryRow, 4),
			}
			svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})
			settings := NewSettingService(&overlapMonitorSettings{}, tc.cfg)
			runner := ProvideChannelMonitorRunner(svc, settings, nil, tc.cfg)
			require.NotNil(t, runner)
			t.Cleanup(runner.Stop)

			if tc.automatic {
				select {
				case rows := <-repo.history:
					require.Len(t, rows, 1)
					require.Equal(t, MonitorStatusOperational, rows[0].Status)
				case <-time.After(2 * time.Second):
					t.Fatal("enabled runner did not complete an automatic probe")
				}
				require.Equal(t, int64(1), probes.Load())
				return
			}

			// Editing an enabled monitor must not schedule work in the disabled process.
			newName := "edited local probe"
			updated, err := svc.Update(context.Background(), repo.monitor.ID, ChannelMonitorUpdateParams{Name: &newName})
			require.NoError(t, err)
			require.Equal(t, newName, updated.Name)
			require.Zero(t, runnerTaskCount(runner))
			require.Never(t, func() bool { return probes.Load() != 0 }, 100*time.Millisecond, 5*time.Millisecond)

			results, err := svc.RunCheck(context.Background(), repo.monitor.ID)
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Equal(t, MonitorStatusOperational, results[0].Status)
			require.Equal(t, int64(1), probes.Load(), "manual check still reaches the upstream once")
			select {
			case rows := <-repo.history:
				require.Len(t, rows, 1)
				require.Equal(t, repo.monitor.ID, rows[0].MonitorID)
			default:
				t.Fatal("manual check did not persist its result")
			}
		})
	}
}
