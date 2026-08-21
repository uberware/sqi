// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"time"

	"github.com/uberware/sqi/internal/api"
)

// routerConfig maps this server's [Config] onto the HTTP-layer [api.Config]
// that [api.NewRouter] is built with.
//
// It is a function rather than an inline literal in start for the reason
// ExprLimitsFromConfig is: a struct literal assigning one config's fields into
// another's is the shape where an assignment can be dropped, renamed, or
// pointed at the wrong field and still compile, start, and serve. Left inline
// it could only be reached by booting a whole server (NATS, SQLite, a
// listening socket), so in practice nothing tested it at all —
// TestRouterConfig_CarriesTheSubmissionDeadline does now.
//
// workerOfflineThreshold comes from the running scheduler rather than from
// Config, which is why it is a parameter: the scheduler normalizes its own
// heartbeat timeout, and the worker handler must gate on the value actually in
// force, not on the one the operator wrote down.
func routerConfig(cfg Config, workerOfflineThreshold time.Duration) api.Config {
	return api.Config{
		CORSOrigins:            cfg.CORSOrigins,
		EnablePprof:            cfg.EnablePprof,
		DisableRateLimit:       cfg.DisableRateLimit,
		WorkerOfflineThreshold: workerOfflineThreshold,
		AuthEnabled:            cfg.AuthEnabled,
		ValidateJobOwner:       cfg.AuthValidateJobOwner,
		ExprSubmissionDeadline: cfg.OpenJDExprSubmissionDeadline,
		ExprLimits:             cfg.OpenJDExprLimits,
	}
}

// natsAuthDeps copies this server's broker-auth-derived settings onto deps:
// whether POST /api/v1/workers/enroll is mounted at all, and the join-token
// defaults its handlers consult. It mutates deps rather than returning an
// api.Deps, unlike routerConfig, because Deps is built incrementally across
// several steps in start (wireAuthDeps, the preset library, diagnostics) and
// a returned value here would either overwrite those or need its own merge.
//
// Split out for the same reason routerConfig is (see its doc comment): a
// struct-field assignment inline in start can only be reached by booting a
// whole server, so nothing would catch a dropped or transposed line.
//
// Nothing else fails when this hop breaks: the server still boots, `config
// print` still echoes the operator's values, and the route simply never
// mounts — a deployed server would run with all four at their zero value
// regardless of nats.auth.* configuration, the enrollment endpoint never
// mounted and a minted token carrying a zero TTL.
func natsAuthDeps(cfg Config, deps *api.Deps) {
	deps.NATSAuthEnabled = cfg.NATSAuthEnabled
	deps.NATSAuthEnrollmentEndpointEnabled = cfg.NATSAuthEnrollmentEndpointEnabled
	deps.JoinTokenTTL = cfg.NATSAuthJoinTokenTTL
	deps.JoinTokenSingleUse = cfg.NATSAuthJoinTokenSingleUse
}
