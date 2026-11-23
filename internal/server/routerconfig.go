// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"github.com/uberware/sqi/internal/api"
)

// routerConfig maps this server's [Config] onto the HTTP-layer [api.Config]
// that [api.NewRouter] is built with.
//
// It is a function rather than an inline literal in start, as
// ExprLimitsFromConfig is, so a test can check it without booting a server
// (NATS, SQLite, a listening socket): a struct literal copying one config's
// fields into another's can drop or misassign a field and still compile,
// start and serve. TestRouterConfig_CarriesTheSubmissionDeadline covers it.
func routerConfig(cfg Config) api.Config {
	return api.Config{
		CORSOrigins:            cfg.CORSOrigins,
		EnablePprof:            cfg.EnablePprof,
		DisableRateLimit:       cfg.DisableRateLimit,
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
// It is split out so a test can check it, for the reason routerConfig is.
// Nothing else fails when a line here is dropped: the server boots, `config
// print` echoes the operator's values, and the handlers see the zero value
// whatever nats.auth.* says, so the enrollment endpoint is not mounted and a
// minted token carries a zero TTL.
func natsAuthDeps(cfg Config, deps *api.Deps) {
	deps.NATSAuthEnabled = cfg.NATS.Auth.Enabled
	deps.NATSAuthEnrollmentEndpointEnabled = cfg.NATS.Auth.EnrollmentEndpointEnabled
	deps.JoinTokenTTL = cfg.NATS.Auth.JoinTokenTTL
	deps.JoinTokenSingleUse = cfg.NATS.Auth.JoinTokenSingleUse
}
