//go:build unit

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadOverlapStartupOptions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		preserve     string
		probes       string
		wantPreserve bool
		wantProbes   bool
	}{
		{name: "defaults", wantProbes: true},
		{name: "overlap candidate", preserve: "true", probes: "false", wantPreserve: true},
		{name: "explicit ordinary startup", preserve: "false", probes: "true", wantProbes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("GATEWAY_PRESERVE_CONCURRENCY_SLOTS_ON_STARTUP", tc.preserve)
			t.Setenv("CHANNEL_MONITOR_ACTIVE_PROBES_ENABLED", tc.probes)

			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, tc.wantPreserve, cfg.Gateway.PreserveConcurrencySlotsOnStartup)
			require.Equal(t, tc.wantProbes, cfg.ChannelMonitor.ActiveProbesEnabled)
		})
	}
}
