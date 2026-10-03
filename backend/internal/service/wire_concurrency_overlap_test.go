//go:build unit

package service_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestProvideConcurrencyService_StartupSlots(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      *config.Config
		preserve bool
	}{
		{name: "default cleanup", cfg: &config.Config{}},
		{name: "nil config cleanup"},
		{
			name:     "overlap preserves other process",
			cfg:      &config.Config{Gateway: config.GatewayConfig{PreserveConcurrencySlotsOnStartup: true}},
			preserve: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redisServer := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			cache := repository.NewConcurrencyCache(client, 15, 900)
			ctx := context.Background()
			previousRequest := "previous-process:in-flight"

			acquired, err := cache.AcquireAccountSlot(ctx, 10, 1, previousRequest)
			require.NoError(t, err)
			require.True(t, acquired)
			acquired, err = cache.AcquireUserSlot(ctx, 20, 1, previousRequest)
			require.NoError(t, err)
			require.True(t, acquired)
			acquired, err = cache.IncrementAccountWaitCount(ctx, 10, 2)
			require.NoError(t, err)
			require.True(t, acquired)
			acquired, err = cache.IncrementWaitCount(ctx, 20, 2)
			require.NoError(t, err)
			require.True(t, acquired)

			svc := service.ProvideConcurrencyService(cache, nil, tc.cfg)
			wantCount := 0
			if tc.preserve {
				wantCount = 1
			}
			accountCount, err := cache.GetAccountConcurrency(ctx, 10)
			require.NoError(t, err)
			require.Equal(t, wantCount, accountCount)
			userCount, err := cache.GetUserConcurrency(ctx, 20)
			require.NoError(t, err)
			require.Equal(t, wantCount, userCount)
			accountWaiting, err := cache.GetAccountWaitingCount(ctx, 10)
			require.NoError(t, err)
			require.Equal(t, wantCount, accountWaiting)
			userWaiting, err := client.Get(ctx, "concurrency:wait:20").Int()
			require.True(t, err == nil || err == redis.Nil)
			require.Equal(t, wantCount, userWaiting)

			// Preserved work must still consume capacity for requests in the candidate.
			slot, err := svc.AcquireAccountSlot(ctx, 10, 1)
			require.NoError(t, err)
			require.Equal(t, !tc.preserve, slot.Acquired)
			if slot.ReleaseFunc != nil {
				slot.ReleaseFunc()
			}
			slot, err = svc.AcquireUserSlot(ctx, 20, 1)
			require.NoError(t, err)
			require.Equal(t, !tc.preserve, slot.Acquired)
			if slot.ReleaseFunc != nil {
				slot.ReleaseFunc()
			}
		})
	}
}

func TestProvideConcurrencyService_PreserveKeepsTimestampCleanup(t *testing.T) {
	// The existing worker has no stop API. Isolate it so the test leaves no ticker or goroutine.
	if os.Getenv("SUB2_OVERLAP_CLEANUP_CHILD") != "1" {
		executable, err := os.Executable()
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProvideConcurrencyService_PreserveKeepsTimestampCleanup$")
		cmd.Env = append(os.Environ(), "SUB2_OVERLAP_CLEANUP_CHILD=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		return
	}
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	cache := repository.NewConcurrencyCache(client, 15, 900)
	ctx := context.Background()
	now := time.Now().Unix()
	require.NoError(t, client.ZAdd(ctx, "concurrency:account:10",
		redis.Z{Score: float64(now), Member: "previous-process:live"},
		redis.Z{Score: float64(now - 901), Member: "previous-process:expired"},
	).Err())
	require.NoError(t, client.ZAdd(ctx, "concurrency:account:active_index",
		redis.Z{Score: float64(now - 1), Member: "10"},
	).Err())

	cfg := &config.Config{Gateway: config.GatewayConfig{PreserveConcurrencySlotsOnStartup: true}}
	// The existing worker runs cleanup immediately, then once per interval.
	// The child process exits before the next tick.
	cfg.Gateway.Scheduling.SlotCleanupInterval = time.Hour
	service.ProvideConcurrencyService(cache, nil, cfg)
	require.Eventually(t, func() bool {
		members, err := client.ZRange(ctx, "concurrency:account:10", 0, -1).Result()
		return err == nil && len(members) == 1 && members[0] == "previous-process:live"
	}, time.Second, 5*time.Millisecond)
}
