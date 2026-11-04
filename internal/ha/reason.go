// SPDX-License-Identifier: AGPL-3.0-or-later

package ha

import "fmt"

// Reason names why a leader lost its authority. It is the label on the
// demotion metric and the word in the one ERROR a demoted leader logs.
type Reason string

const (
	// ReasonRenewRefused means a renewal found the lease held by another
	// server or at another epoch: a standby has already taken over.
	ReasonRenewRefused Reason = "renew_refused"
	// ReasonDeadline means the self-fence deadline passed with no successful
	// renewal, so a standby may be about to take over.
	ReasonDeadline Reason = "deadline"
	// ReasonFenced means a store write was refused for a stale epoch.
	ReasonFenced Reason = "fenced"
	// ReasonGuardLost means another server took the broker guard.
	ReasonGuardLost Reason = "guard_lost"
	// ReasonBrokerLost means the leader lost its broker for longer than the
	// lease while a standby still reaches it, so the leader stepped down.
	ReasonBrokerLost Reason = "broker_lost"
	// ReasonPromotion means a step of taking up leadership failed.
	ReasonPromotion Reason = "promotion_failed"
)

// LostError reports a loss of leadership. Once returned, the [Elector] that
// returned it never leads again: the server exits and restarts as a standby.
type LostError struct {
	Reason Reason
	// Err is the cause, when there is one beyond the reason itself.
	Err error
}

// Error names the reason and the cause.
func (e *LostError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("ha: lost leadership (%s)", e.Reason)
	}
	return fmt.Sprintf("ha: lost leadership (%s): %v", e.Reason, e.Err)
}

// Unwrap returns the cause.
func (e *LostError) Unwrap() error { return e.Err }
