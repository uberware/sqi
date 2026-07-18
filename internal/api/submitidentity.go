// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Job identity binding for the two submission entry points (POST /jobs and
// POST /products/{name}/jobs). Both route through bindSubmitIdentity so the
// rules cannot drift apart.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
	"github.com/uberware/sqi/internal/store"
)

// ownerLookup reports whether username names a known user. It returns
// store.ErrNotFound for an unknown user and nil to accept. A nil ownerLookup
// disables validation (auth.validate_job_owner = false).
type ownerLookup func(ctx context.Context, username string) error

// newOwnerLookup returns an ownerLookup backed by st, or nil when owner
// validation is disabled (a nil lookup makes bindSubmitIdentity skip the check).
func newOwnerLookup(st store.Store, validate bool) ownerLookup {
	if !validate {
		return nil
	}
	return func(ctx context.Context, username string) error {
		_, err := st.GetUserByUsername(ctx, username)
		return err
	}
}

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
//  4. Owner other than self requires policy.JobsSubmitAs, else 403. When
//     granted, lookup (if non-nil) must also confirm the named owner is a
//     known user, else 400 — this keeps Job.Owner a trustworthy key.
//
// When auth is disabled the principal is the anonymous superuser: it holds
// every permission and carries no username, so both client values pass through
// verbatim and behavior is byte-for-byte what it was pre-B2. That passthrough
// is keyed on the principal actually being the anonymous/auth-off identity
// (auth.KindAnonymous), never on an empty Username — a future authenticator
// could hand back an authenticated, permission-bearing principal with no
// local username, and such a principal must still be run through the
// jobs.submit_as check rather than silently bypassing it.
//
// A zero status means success. Otherwise problem carries the message and
// status the HTTP code the caller should write.
func bindSubmitIdentity(
	ctx context.Context, lookup ownerLookup, clientOwner, clientSubmitter string,
) (owner, submitter, problem string, status int) {
	p, ok := auth.FromContext(ctx)
	if !ok || p.Kind == auth.KindAnonymous {
		// No authenticated identity to bind to (auth disabled). Preserve
		// today's client-supplied behavior.
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

	if lookup != nil {
		if err := lookup(ctx, owner); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "", "", "owner names no known user", http.StatusBadRequest
			}
			return "", "", "failed to validate owner", http.StatusInternalServerError
		}
	}
	return owner, submitter, "", 0
}
