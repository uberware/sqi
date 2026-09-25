// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"testing"

	"github.com/uberware/sqi/internal/server"
)

func TestServerDrainTimeout_IsShutdownTimeout(t *testing.T) {
	got, err := serverDrainTimeout("")
	if err != nil || got != server.ShutdownTimeout {
		t.Fatalf("serverDrainTimeout = %v, %v; want %v", got, err, server.ShutdownTimeout)
	}
}

func TestServerServiceCommand_Registered(t *testing.T) {
	for _, sub := range []string{"install", "uninstall", "start", "stop", "status"} {
		if c, _, err := rootCmd.Find([]string{"service", sub}); err != nil || c.Name() != sub {
			t.Errorf("service %s not registered: %v", sub, err)
		}
	}
}
