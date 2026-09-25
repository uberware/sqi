// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/winsvc"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

func init() {
	rootCmd.AddCommand(winsvc.NewCommand(winsvc.CommandSpec{
		Binary:           "sqi-worker",
		RunVerb:          "start",
		DisplayName:      "sqi Worker Agent",
		Description:      "sqi distributed task worker",
		DelayedAutoStart: true,
		DrainTimeout:     workerDrainTimeout,
		// Printed whenever --user names an account: isolation is queue-scoped
		// (queues.run_as_user), so the worker config cannot tell whether
		// run-as-user will be used.
		UserWarning: "if this farm uses run-as-user queues, the account also needs " +
			"SeAssignPrimaryTokenPrivilege (LocalSystem holds it) — see docs/worker-configuration.md#windows",
	}))
}

// workerDrainTimeout reads worker.shutdown_grace_period from configPath, with
// SQI_WORKER_* overrides from the installing shell applied. The service itself
// sees the system environment, not that shell's.
func workerDrainTimeout(configPath string) (time.Duration, error) {
	cfg, err := workerconfig.Load(configPath, workerconfig.FlagOverrides{})
	if err != nil {
		return 0, fmt.Errorf("load %s: %w", configPath, err)
	}
	return cfg.Worker.ShutdownGracePeriod, nil
}
