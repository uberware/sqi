// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"time"

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
	}))
}

// serverDrainTimeout is the server's graceful-shutdown bound. It is a
// constant today, so the config file is not consulted.
func serverDrainTimeout(string) (time.Duration, error) { return server.ShutdownTimeout, nil }
