// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/winsvc"
)

func init() {
	rootCmd.AddCommand(winsvc.NewCommand(winsvc.CommandSpec{
		Binary:       "sqi-server",
		RunVerb:      "serve",
		DisplayName:  "sqi Server",
		Description:  "sqi distributed task server (scheduler, REST API, embedded NATS)",
		DrainTimeout: serverDrainTimeout,
		ConfigPaths:  serverConfigPaths,
	}))
}

// serverConfigPaths is the log.file and every path the configuration in
// configPath names, with SQI_* overrides from the installing shell applied,
// for service install to check.
func serverConfigPaths(configPath, _ string) (winsvc.ConfigPaths, error) {
	cfg, err := config.Load(configPath, config.FlagOverrides{})
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

// serverDrainTimeout is the server's graceful-shutdown bound. It is a
// constant today, so the config file is not consulted.
func serverDrainTimeout(string) (time.Duration, error) { return server.ShutdownTimeout, nil }
