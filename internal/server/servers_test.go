// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"os"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/version"
)

func TestServersConfig_WithoutHAIsThisServer(t *testing.T) {
	s := New(Config{HA: config.HAConfig{InstanceName: "farm-1"}}, nil, nil)
	before := time.Now().UTC().Add(-time.Second)
	got := s.serversConfig()
	if got.HA {
		t.Error("HA = true without ha.enabled")
	}
	self := got.Self
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if self.Name != "farm-1" || self.Host != host || self.PID != os.Getpid() || self.Version != version.Get().Version {
		t.Errorf("self = %+v", self)
	}
	if self.InstanceID == "" {
		t.Error("self has no instance id")
	}
	if self.StartedAt.Before(before) || self.StartedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("started_at = %v, want about now", self.StartedAt)
	}
}

func TestServersConfig_NameDefaultsToTheHostName(t *testing.T) {
	got := New(Config{}, nil, nil).serversConfig()
	if got.Self.Name == "" {
		t.Error("name is empty, want the host name")
	}
}

func TestServersConfig_WithHAReadsTheGroup(t *testing.T) {
	s := New(Config{HA: config.HAConfig{Enabled: true, LeaseTTL: 20 * time.Second}}, nil, nil)
	got := s.serversConfig()
	if !got.HA || got.LeaseTTL != 20*time.Second {
		t.Errorf("config = %+v, want HA with a 20s lease ttl", got)
	}
}
