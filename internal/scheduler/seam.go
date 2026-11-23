// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// seam.go — exported entry points that exist for out-of-package test harnesses.
//
// internal/presettest builds an argv snapshot for every shipped preset by
// driving the production path: submit the template, build the assignment,
// resolve it as a worker would. Re-implementing any of those steps in a test
// helper would produce goldens that agree with the helper rather than with the
// server. So the harness calls production code, which this file exports as a
// thin delegation with no logic of its own.

import (
	"context"

	"github.com/uberware/sqi/internal/store"
)

// BuildAssignPayload is [buildAssignPayload] for callers outside this package.
//
// It delegates verbatim and adds nothing. Keep it that way: the unexported
// function stays the single implementation, so a change to assignment building
// cannot reach production without also reaching the preset goldens.
func BuildAssignPayload(
	ctx context.Context,
	task store.Task,
	worker store.Worker,
	job store.Job,
	step store.Step,
	queue store.Queue,
	attemptID string,
	locStore store.StorageLocationStore,
) ([]byte, error) {
	return buildAssignPayload(ctx, task, worker, job, step, queue, attemptID, locStore)
}
