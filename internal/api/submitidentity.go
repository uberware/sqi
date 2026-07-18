// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Job identity binding for the two submission entry points (POST /jobs and
// POST /products/{name}/jobs). Both route through bindSubmitIdentity so the
// rules cannot drift apart.

import (
	"context"
	"net/http"
	"strings"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
)

// bindSubmitIdentity resolves the Owner and Submitter to persist for a
// submission, given whatever the client supplied.
//
// Precedence:
//  1. Submitter is always the principal's username. A client value is
//     discarded silently, never an error — a client asserting its own identity
//     is meaningless rather than hostile, and erroring would break every
//     existing submitter the moment auth is switched on.
//  2. Owner defaults to Submitter when the client supplies none.
//  3. Owner equal to self (case-insensitive) is accepted; the principal's own
//     canonical casing is stored, not the client's.
//  4. Owner other than self requires policy.JobsSubmitAs, else 403.
//
// When auth is disabled the principal is the anonymous superuser: it holds
// every permission and carries no username, so both client values pass through
// verbatim and behavior is byte-for-byte what it was pre-B2.
//
// A zero status means success. Otherwise problem carries the message and
// status the HTTP code the caller should write.
func bindSubmitIdentity(
	ctx context.Context, clientOwner, clientSubmitter string,
) (owner, submitter, problem string, status int) {
	p, ok := auth.FromContext(ctx)
	if !ok || p.Username == "" {
		// No authenticated username to bind to (auth disabled, or a principal
		// kind that carries none). Preserve today's client-supplied behavior.
		return clientOwner, clientSubmitter, "", 0
	}

	submitter = p.Username
	owner = strings.TrimSpace(clientOwner)
	if owner == "" || strings.EqualFold(owner, p.Username) {
		return p.Username, submitter, "", 0
	}

	if !policy.Can(p, policy.JobsSubmitAs) {
		return "", "", "setting owner to another user requires the jobs.submit_as permission",
			http.StatusForbidden
	}
	return owner, submitter, "", 0
}
