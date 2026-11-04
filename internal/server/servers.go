// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"os"
	"time"

	"github.com/uberware/sqi/internal/api"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/version"
)

// serversConfig is what GET /api/v1/servers needs: with HA, the lease TTL to
// judge the group's members by; without, this server's own identity, which
// it reports as the one server of the farm. Its instance id and start time
// are made here, since a server without HA keeps no membership row.
func (s *Server) serversConfig() api.ServersConfig {
	if s.cfg.HA.Enabled {
		return api.ServersConfig{HA: true, LeaseTTL: s.cfg.HA.LeaseTTL}
	}
	self := s.identity()
	return api.ServersConfig{Self: api.ServerSelf{
		InstanceID: self.InstanceID,
		Name:       self.Name,
		Host:       self.Host,
		PID:        self.PID,
		Version:    self.Version,
		StartedAt:  self.StartedAt,
	}}
}

// identity is this process as a server of the farm, with or without HA. The
// instance id is new on every start, so each process is a server of its own.
// It leaves out the grants fingerprint, which only an HA member records
// ([Server.haSelf]).
func (s *Server) identity() store.ServerMember {
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	return store.ServerMember{
		InstanceID: bus.NewInstanceID(),
		Name:       s.cfg.HA.Name(),
		Host:       host,
		PID:        os.Getpid(),
		Version:    version.Get().Version,
		StartedAt:  time.Now().UTC(),
	}
}
