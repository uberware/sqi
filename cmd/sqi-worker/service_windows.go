// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"fmt"
	"path/filepath"
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

// workerConfigPaths is every path the configuration in configPath names,
// with SQI_WORKER_* overrides from the installing shell applied, for service
// install to check, plus the session root the worker uses when
// worker.session_dir is unset.
//
// It refuses a configuration that leaves worker.data_dir unset. The default
// is under the home directory of the account that loads it: here the
// installing administrator's, for the service its own account's (for
// LocalSystem, C:\Windows\System32\config\systemprofile), so the path install
// would check is not the one the worker keeps its identity and credential in.
//
// For an account ("" is LocalSystem) it also refuses a session root under
// %ProgramData%\sqi (see userSessionRoot).
func workerConfigPaths(configPath, account string) ([]winsvc.PathRef, error) {
	cfg, src, err := workerconfig.LoadWithSources(configPath, workerconfig.FlagOverrides{})
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", configPath, err)
	}
	if !src.DataDir {
		return nil, fmt.Errorf("%s does not set worker.data_dir, so a service would keep its "+
			"identity and credential under its own account's profile, which install cannot check. "+
			"Set it explicitly, for example:\n  worker:\n    data_dir: %s\nor write a new configuration "+
			"with sqi-worker config init", configPath, filepath.Join(filepath.Dir(configPath), "worker"))
	}
	if err := userSessionRoot(configPath, account, cfg.Worker); err != nil {
		return nil, err
	}
	named := cfg.NamedPaths()
	paths := make([]winsvc.PathRef, 0, len(named)+1)
	for _, p := range named {
		paths = append(paths, winsvc.PathRef(p))
	}
	if cfg.Worker.SessionDir == "" {
		root, _ := effectiveSessionRoot(cfg.Worker)
		paths = append(paths, winsvc.PathRef{Key: "worker.session_dir (default)", Path: root})
	}
	return paths, nil
}

// userSessionRoot refuses, for a service running as account, a session root
// under %ProgramData%\sqi: the default when worker.session_dir is unset, or
// one set there. Only SYSTEM and Administrators can write that directory, so
// the worker would register and then fail every task it was given, creating
// its session directory. LocalSystem ("") can use it.
func userSessionRoot(configPath, account string, w workerconfig.WorkerSettings) error {
	if account == "" {
		return nil
	}
	root, _ := effectiveSessionRoot(w)
	if !filepath.IsAbs(root) {
		root = filepath.Join(filepath.Dir(configPath), root)
	}
	sqiDir := winsvc.SqiDir()
	if !winsvc.PathWithin(root, sqiDir) {
		return nil
	}
	what := "leaves worker.session_dir unset, so the worker would create its task session directories in " + root
	if w.SessionDir != "" {
		what = "sets worker.session_dir to " + root
	}
	return fmt.Errorf("%s %s, which is under %s, where only SYSTEM and Administrators can write: running as %s, "+
		"the worker would fail every task. Set worker.session_dir to a directory %s owns or can write, for example:\n"+
		"  worker:\n    session_dir: %s\n(sqi-worker config init --user writes and creates it)",
		configPath, what, sqiDir, account, account, filepath.Join(filepath.Dir(configPath), "sessions"))
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
