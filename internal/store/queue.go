// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"time"
)

// ErrRunAsGroupWithoutUser is returned by [QueueStore.CreateQueue] and
// [QueueStore.UpdateQueue] when the RESOLVED run_as_group (after any
// PreserveRunAsGroup substitution) would be non-empty while the resolved
// run_as_user (after any PreserveRunAsUser substitution) is nil or empty.
// The scheduler only gates isolation on RunAsUser (internal/scheduler/
// assign.go builds protocol.IsolationSpec only when queue.RunAsUser is set,
// folding RunAsGroup in only as a supplement) — so a group with no user
// selects no OS identity at all and is silently ignored, giving an operator
// no isolation and no warning that their configuration did nothing. Checking
// the resolved values (not the raw request) matters for UpdateQueue: a PUT
// that clears run_as_user while an existing run_as_group is preserved, or
// that sets run_as_group while run_as_user is preserved at nil, must be
// rejected exactly like setting both in the same request.
var ErrRunAsGroupWithoutUser = errors.New("store: run_as_group requires run_as_user")

// RunAsComboValid reports whether a resolved run_as_user/run_as_group pair is
// a valid queue isolation identity: nil/empty is always valid (no isolation
// configured); a non-empty group additionally requires a non-empty user.
func RunAsComboValid(user, group *string) bool {
	hasGroup := group != nil && *group != ""
	hasUser := user != nil && *user != ""
	return !hasGroup || hasUser
}

// Queue belongs to a [Farm] and is the container into which jobs are submitted.
// Scheduling policy (priority, concurrency limits, paused state) is configured
// at the queue level and may be overridden per job.
type Queue struct {
	ID                 string
	FarmID             string
	Name               string
	Description        string
	Priority           int
	MaxConcurrentTasks int // 0 = unlimited
	Paused             bool
	// MaxAttempts, RetryDelaySeconds, and FailureLimit are queue-level retry
	// policy overrides. Nil means "inherit" (Farm -> server default).
	MaxAttempts       *int
	RetryDelaySeconds *int
	FailureLimit      *int
	// RunAsUser and RunAsGroup are the OS identity that tasks in this queue
	// execute as. Nil RunAsUser means no isolation: tasks run as the worker's
	// own user, which is the pre-isolation behavior. Nil RunAsGroup means the
	// target user's primary group.
	//
	// Deliberately NOT part of the Queue -> Farm -> server-default cascade used
	// by the retry-policy fields above: a farm-wide default would silently
	// apply an OS identity to queues whose owner never asked for one.
	RunAsUser  *string
	RunAsGroup *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// PreserveRunAsUser and PreserveRunAsGroup are read only by
	// [QueueStore.UpdateQueue] (ignored by CreateQueue and never populated by a
	// read). When true, the update leaves the corresponding column untouched —
	// RunAsUser/RunAsGroup on this struct is ignored for that column — instead
	// of overwriting it with the given (possibly zero) value.
	//
	// This exists so the API layer's "key omitted from the PUT body means
	// preserve the stored identity" rule can be expressed as a single atomic
	// UPDATE statement instead of a read-then-write: a prior GetQueue to fetch
	// the existing value followed by a separate UpdateQueue left a window in
	// which a concurrent admin write setting the identity could be silently
	// undone by an unrelated operator write that started before it (lost
	// update). Preferring "leave this column alone" inside the same statement
	// closes that race outright rather than narrowing it.
	PreserveRunAsUser  bool
	PreserveRunAsGroup bool
}

// QueueSortField is a column by which [QueueStore.ListQueues] results can be
// ordered.
type QueueSortField string

const (
	// QueueSortByName orders queues alphabetically by name (default).
	QueueSortByName QueueSortField = "name"
	// QueueSortByPriority orders queues by priority (higher values first when
	// SortDesc).
	QueueSortByPriority QueueSortField = "priority"
	// QueueSortByCreatedAt orders queues by creation time.
	QueueSortByCreatedAt QueueSortField = "created_at"
)

// QueueStore is the persistence interface for [Queue] records.
type QueueStore interface {
	// CreateQueue inserts a new queue. Returns [ErrConflict] if a queue with
	// the same name already exists within the same farm.
	CreateQueue(ctx context.Context, queue Queue) (Queue, error)

	// GetQueue returns the queue with the given ID, or [ErrNotFound].
	GetQueue(ctx context.Context, id string) (Queue, error)

	// ListQueues returns a paginated, filtered, and sorted page of queues
	// matching opts. Call [Pagination.Validate] on opts.Pagination before
	// passing it to ensure sensible defaults are applied.
	ListQueues(ctx context.Context, opts ListQueuesOptions) (Page[Queue], error)

	// UpdateQueue replaces the mutable fields of an existing queue and updates
	// UpdatedAt, in a single atomic write. RunAsUser/RunAsGroup are the
	// exception: when queue.PreserveRunAsUser (resp. PreserveRunAsGroup) is
	// true, that column is left at its stored value instead of being
	// overwritten by queue.RunAsUser (resp. queue.RunAsGroup) — see the
	// [Queue.PreserveRunAsUser] doc for why this exists. Returns [ErrNotFound]
	// or [ErrConflict] as appropriate.
	UpdateQueue(ctx context.Context, queue Queue) (Queue, error)

	// DeleteQueue removes a queue by ID. Returns [ErrNotFound] if it does not
	// exist. The caller is responsible for ensuring no jobs reference the
	// queue before deleting it.
	DeleteQueue(ctx context.Context, id string) error
}

// ListQueuesOptions filters and orders [QueueStore.ListQueues] results.
// Zero values mean "no filter / use defaults".
type ListQueuesOptions struct {
	// Filters
	FarmID string
	Paused *bool // nil = all, true = paused only, false = active only

	// Ordering — zero values use QueueSortByName / SortAsc.
	SortBy  QueueSortField
	SortDir SortDir

	// Pagination — call Pagination.Validate() before use.
	Pagination Pagination
}
