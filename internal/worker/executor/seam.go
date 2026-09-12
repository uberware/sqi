// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

// seam.go — exported entry point that exists for out-of-package test harnesses.
// See internal/scheduler/seam.go for the reasoning; this is its other half.

import (
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
)

// ResolveAssignment is [resolveAssignment] for callers outside this package: it
// returns the resolved OnRun action, the session's static environment, and the
// resolved embedded files for msg, exactly as the worker computes them before
// launching a process.
//
// It delegates verbatim and adds nothing. internal/presettest renders the
// result into a reviewed golden file per shipped preset, so anything this
// wrapper did on its own would be a difference between what the goldens show and
// what a worker runs.
func ResolveAssignment(
	msg *protocol.AssignMsg,
	sess *session.Session,
) (*protocol.Action, map[string]string, []protocol.EmbeddedFile, error) {
	return resolveAssignment(msg, sess)
}
