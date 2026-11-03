// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import "time"

// Worker liveness timing. The worker paces its heartbeats and its broker
// redials by these, and the server sizes the grace its liveness sweeps keep
// after a bus outage from them: a live worker may take the longest redial
// wait, with its jitter, and then one heartbeat interval to be heard from
// again. They live here, in the leaf both binaries share, so that sqi-server
// does not link the worker's configuration or broker-client packages to read
// three numbers.
const (
	// MaxReconnectBackoff is the upper bound on a worker's broker redial
	// delay, however many attempts have elapsed.
	MaxReconnectBackoff = 2 * time.Minute

	// ReconnectJitterFraction is how much randomness is added to each redial
	// delay. At 0.2 the actual delay is the base ± 20%.
	ReconnectJitterFraction = 0.2

	// DefaultHeartbeatInterval is how often a worker publishes a heartbeat
	// unless worker.heartbeat_interval says otherwise.
	DefaultHeartbeatInterval = 15 * time.Second
)
