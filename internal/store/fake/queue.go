// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateQueue inserts a new queue. Returns [store.ErrConflict] if a queue with
// the same name already exists within the same farm.
func (s *Store) CreateQueue(_ context.Context, queue store.Queue) (store.Queue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.queues {
		if existing.FarmID == queue.FarmID && existing.Name == queue.Name {
			return store.Queue{}, store.ErrConflict
		}
	}

	s.queues[queue.ID] = queue
	return queue, nil
}

// GetQueue returns the queue with the given ID, or [store.ErrNotFound].
func (s *Store) GetQueue(_ context.Context, id string) (store.Queue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue, ok := s.queues[id]
	if !ok {
		return store.Queue{}, store.ErrNotFound
	}
	return queue, nil
}

// ListQueues returns a paginated, filtered, and sorted page of queues
// matching opts.
func (s *Store) ListQueues(_ context.Context, opts store.ListQueuesOptions) (store.Page[store.Queue], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := opts.Pagination.Validate(); err != nil {
		return store.Page[store.Queue]{}, err
	}

	queues := make([]store.Queue, 0, len(s.queues))
	for _, q := range s.queues {
		if filterQueue(q, opts) {
			queues = append(queues, q)
		}
	}

	slices.SortStableFunc(queues, func(a, b store.Queue) int {
		return cmpQueue(a, b, opts.SortBy, opts.SortDir)
	})

	return applyPage(queues, opts.Pagination), nil
}

// UpdateQueue replaces the mutable fields of an existing queue and updates
// UpdatedAt. Returns [store.ErrNotFound] or [store.ErrConflict] as appropriate.
func (s *Store) UpdateQueue(_ context.Context, queue store.Queue) (store.Queue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.queues[queue.ID]
	if !ok {
		return store.Queue{}, store.ErrNotFound
	}

	// Check name+farm uniqueness, excluding self.
	if queue.Name != existing.Name || queue.FarmID != existing.FarmID {
		for _, q := range s.queues {
			if q.ID != queue.ID && q.FarmID == queue.FarmID && q.Name == queue.Name {
				return store.Queue{}, store.ErrConflict
			}
		}
	}

	queue.CreatedAt = existing.CreatedAt
	queue.UpdatedAt = time.Now()
	s.queues[queue.ID] = queue
	return queue, nil
}

// DeleteQueue removes a queue by ID. Returns [store.ErrNotFound] if it does
// not exist.
func (s *Store) DeleteQueue(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.queues[id]; !ok {
		return store.ErrNotFound
	}

	delete(s.queues, id)
	return nil
}

// filterQueue reports whether q matches all non-zero filter fields in opts.
func filterQueue(q store.Queue, opts store.ListQueuesOptions) bool {
	if opts.FarmID != "" && q.FarmID != opts.FarmID {
		return false
	}
	if opts.Paused != nil && q.Paused != *opts.Paused {
		return false
	}
	return true
}

// cmpQueue returns a comparison value for two queues by the given sort field
// and direction. An empty field defaults to [store.QueueSortByName]; an empty
// direction defaults to ascending.
func cmpQueue(a, b store.Queue, field store.QueueSortField, dir store.SortDir) int {
	var n int
	switch field {
	case store.QueueSortByPriority:
		n = cmp.Compare(a.Priority, b.Priority)
	case store.QueueSortByCreatedAt:
		n = a.CreatedAt.Compare(b.CreatedAt)
	default: // QueueSortByName
		n = cmp.Compare(a.Name, b.Name)
	}
	if dir == store.SortDesc {
		return -n
	}
	return n
}
