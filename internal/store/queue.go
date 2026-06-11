// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

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
	CreatedAt          time.Time
	UpdatedAt          time.Time
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
	// UpdatedAt. Returns [ErrNotFound] or [ErrConflict] as appropriate.
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
