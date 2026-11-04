// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Servers REST handler.
//
// Route summary:
//
//	GET /api/v1/servers — the servers of the farm's high-availability group

import (
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/uberware/sqi/internal/ha"
	"github.com/uberware/sqi/internal/store"
)

// ServersConfig is what GET /api/v1/servers needs to know about the server
// serving it. The zero value describes a single server with no identity, which
// is what a router built without HA and without this field reports.
type ServersConfig struct {
	// HA is set on a server in a high-availability group: the response then
	// lists the group's members from the database. Unset, the response is
	// Self alone and the database is not read.
	HA bool

	// LeaseTTL is the group's ha.lease_ttl, which decides whether a member
	// counts as seen and whether its broker is working. Only read with HA.
	LeaseTTL time.Duration

	// Self is this server, reported as the leader when HA is unset.
	Self ServerSelf
}

// ServerSelf is a server's own identity, for a farm without HA.
type ServerSelf struct {
	InstanceID string
	Name       string
	Host       string
	PID        int
	Version    string
	StartedAt  time.Time
}

// serverResponse is one server of the group on the wire.
type serverResponse struct {
	InstanceID         string    `json:"instance_id"`
	Name               string    `json:"name"`
	Host               string    `json:"host"`
	PID                int       `json:"pid"`
	Version            string    `json:"version"`
	Role               string    `json:"role"`
	StartedAt          time.Time `json:"started_at"`
	LastSeenSecondsAgo float64   `json:"last_seen_seconds_ago"`
	BrokerOK           bool      `json:"broker_ok"`
}

// serversResponse is returned by GET /api/v1/servers. Servers is never null.
type serversResponse struct {
	HA      bool             `json:"ha"`
	Epoch   int64            `json:"epoch"`
	Servers []serverResponse `json:"servers"`
}

// serversHandler handles the servers REST endpoint.
type serversHandler struct {
	st     store.LeadershipStore
	cfg    ServersConfig
	logger *slog.Logger
}

func newServersHandler(st store.LeadershipStore, cfg ServersConfig, logger *slog.Logger) *serversHandler {
	return &serversHandler{st: st, cfg: cfg, logger: logger}
}

// ── GET /api/v1/servers ───────────────────────────────────────────────────────

// listServers returns the servers of the HA group with their roles, leader
// first, and the current leadership epoch. Roles, ages and broker health are
// judged on the database's clock, so they agree whichever server answers.
//
// Without HA it returns this server alone, as the leader at epoch 0. Its
// broker is reported working: the embedded broker runs in this process, and a
// server whose broker had failed would not be answering.
func (h *serversHandler) listServers(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.HA {
		s := h.cfg.Self
		writeJSON(w, http.StatusOK, serversResponse{Servers: []serverResponse{toServerResponse(ha.RosterMember{
			InstanceID: s.InstanceID,
			Name:       s.Name,
			Host:       s.Host,
			PID:        s.PID,
			Version:    s.Version,
			Role:       ha.RoleLeader,
			StartedAt:  s.StartedAt,
			BrokerOK:   true,
		})}})
		return
	}

	roster, err := ha.ReadRoster(r.Context(), h.st, h.cfg.LeaseTTL)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "servers: read the group failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list servers")
		return
	}
	resp := serversResponse{HA: true, Epoch: roster.Epoch, Servers: make([]serverResponse, 0, len(roster.Members))}
	for _, m := range roster.Members {
		resp.Servers = append(resp.Servers, toServerResponse(m))
	}
	writeJSON(w, http.StatusOK, resp)
}

// toServerResponse is m on the wire, its age rounded to the millisecond.
func toServerResponse(m ha.RosterMember) serverResponse {
	return serverResponse{
		InstanceID:         m.InstanceID,
		Name:               m.Name,
		Host:               m.Host,
		PID:                m.PID,
		Version:            m.Version,
		Role:               m.Role,
		StartedAt:          m.StartedAt,
		LastSeenSecondsAgo: math.Round(m.LastSeenAgo.Seconds()*1000) / 1000,
		BrokerOK:           m.BrokerOK,
	}
}
