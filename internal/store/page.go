// SPDX-License-Identifier: AGPL-3.0-only

package store

import "errors"

// ── Pagination ────────────────────────────────────────────────────────────────

const (
	// DefaultLimit is the page size used when the caller does not specify one.
	DefaultLimit = 50
	// MaxLimit is the maximum page size accepted from callers. Requests above
	// this are clamped rather than rejected, matching the behavior of most
	// REST pagination conventions.
	MaxLimit = 1000
)

// Pagination carries cursor-free (limit/offset) pagination parameters.
// Zero values are treated as "use defaults"; call [Pagination.Validate] before
// passing to a store method to ensure limits are applied.
type Pagination struct {
	// Limit is the maximum number of items to return. 0 means use
	// [DefaultLimit]. Values above [MaxLimit] are clamped to [MaxLimit].
	Limit int
	// Offset is the zero-based index of the first item to return.
	Offset int
}

// Validate clamps Limit into the range [1, MaxLimit], applying [DefaultLimit]
// when Limit is zero. Negative Offset is reset to zero. It returns an error
// only for values that cannot be silently corrected (currently none — all
// out-of-range values are clamped).
func (p *Pagination) Validate() error {
	if p.Limit <= 0 {
		p.Limit = DefaultLimit
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return nil
}

// ErrInvalidPagination is returned when Pagination contains values that
// cannot be corrected by clamping (reserved for future use).
var ErrInvalidPagination = errors.New("store: invalid pagination parameters")

// ── Sort direction ────────────────────────────────────────────────────────────

// SortDir is the direction of a sort: ascending or descending.
type SortDir string

const (
	// SortAsc sorts results from smallest to largest (A→Z, oldest→newest).
	SortAsc SortDir = "asc"
	// SortDesc sorts results from largest to smallest (Z→A, newest→oldest).
	SortDesc SortDir = "desc"
)

// Reverse returns the opposite [SortDir].
func (d SortDir) Reverse() SortDir {
	if d == SortDesc {
		return SortAsc
	}
	return SortDesc
}

// ── Page result ───────────────────────────────────────────────────────────────

// Page is the generic result type returned by paginated list queries.
// T is the item type (e.g. Job, Task, Worker).
type Page[T any] struct {
	// Items holds the current page of results. It is never nil; an empty
	// page is represented as an empty slice.
	Items []T
	// Total is the total number of items matching the filter, regardless of
	// the current page window. Used by clients to compute page counts.
	Total int
	// Limit mirrors the effective limit used to produce this page.
	Limit int
	// Offset mirrors the effective offset used to produce this page.
	Offset int
}

// HasMore reports whether there are items beyond the current page window.
func (p Page[T]) HasMore() bool {
	return p.Offset+len(p.Items) < p.Total
}
