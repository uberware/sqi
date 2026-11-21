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
		ConfigPaths:      workerConfigPaths,
		// Printed whenever --user names an account: isolation is queue-scoped
		// (queues.run_as_user), so the worker config cannot tell whether
		// run-as-user will be used.
		UserWarning: "if this farm uses run-as-user queues, the account also needs " +
			"SeAssignPrimaryTokenPrivilege (LocalSystem holds it) — see docs/worker-configuration.md#windows",
	}))
}

// workerDrainTimeout is the worker's drain bound ([workerDrainBound]) for the
// worker.shutdown_grace_period in configPath, with SQI_WORKER_* overrides from
// the installing shell applied. The service itself sees the system
// environment, not that shell's.
func workerDrainTimeout(configPath string) (time.Duration, error) {
	cfg, err := workerconfig.Load(configPath, workerconfig.FlagOverrides{})
	if err != nil {
		return 0, fmt.Errorf("load %s: %w", configPath, err)
	}
	return workerDrainBound(cfg.Worker.ShutdownGracePeriod), nil
}

// workerConfigPaths is the log.file and every path the configuration in
// configPath names, with SQI_WORKER_* overrides from the installing shell
// applied, for service install to check.
func workerConfigPaths(configPath string) (winsvc.ConfigPaths, error) {
	cfg, err := workerconfig.Load(configPath, workerconfig.FlagOverrides{})
	if err != nil {
		return winsvc.ConfigPaths{}, fmt.Errorf("load %s: %w", configPath, err)
	}
	named := cfg.NamedPaths()
	paths := make([]winsvc.PathRef, 0, len(named))
	for _, p := range named {
		paths = append(paths, winsvc.PathRef(p))
	}
	return winsvc.ConfigPaths{LogFile: cfg.Log.File, Paths: paths}, nil
}

// workerDrainBound is how long a worker shutdown can take to drain: up to the
// grace period for running tasks to finish, then, for any still running,
// SIGTERM and the kill window ([shutdownKillGrace]) before SIGKILL. What
// follows is covered by winsvc.ShutdownMargin: the inline try of each task's
// final status (up to status.DefaultAckTimeout), the wait for the statuses
// the broker has not yet stored (up to status.DefaultFlushTimeout, normally
// far less), the deregistration (up to registration.DeregisterFlushTimeout)
// and the NATS drain. TestShutdownAfterDrainFitsServiceMargin pins the sum.
func workerDrainBound(grace time.Duration) time.Duration { return grace + shutdownKillGrace(grace) }
