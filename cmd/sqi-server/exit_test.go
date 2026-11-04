// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/winsvc"
)

// TestExitCode_Demoted: the error a demoted server returns exits with
// server.ExitDemoted, and the loss on its own does not. winsvc's tests cover
// the mapping in general.
func TestExitCode_Demoted(t *testing.T) {
	lost := &ha.LostError{Reason: ha.ReasonFenced, Err: errors.New("stale epoch")}
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"demoted", server.ErrDemoted, server.ExitDemoted},
		{"demotion with its loss", fmt.Errorf("%w: %w", server.ErrDemoted, lost), server.ExitDemoted},
		{"a loss alone is not a demotion", lost, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := winsvc.ExitCode(tt.err); got != tt.want {
				t.Fatalf("winsvc.ExitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestServerConfig_CarriesHA(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.HA.Enabled = true
	cfg.HA.LeaseTTL = 20 * time.Second
	cfg.HA.InstanceName = "alpha"
	got := serverConfig(cfg, scheduler.Config{}).HA
	if !got.Enabled || got.LeaseTTL != cfg.HA.LeaseTTL || got.InstanceName != "alpha" {
		t.Fatalf("server HA = %+v; want %+v", got, cfg.HA)
	}
}

// TestServerConfig_HAOffByDefault: the default configuration starts no HA.
func TestServerConfig_HAOffByDefault(t *testing.T) {
	if got := serverConfig(config.DefaultConfig(), scheduler.Config{}).HA; got.Enabled {
		t.Fatalf("default server HA = %+v; want disabled", got)
	}
}
