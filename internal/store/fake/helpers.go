// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"maps"

	"github.com/uberware/sqi/internal/store"
)

// applyPage takes a pre-sorted slice and pagination parameters, and returns
// a Page[T] with the correct Total, Items, Limit, and Offset values.
func applyPage[T any](items []T, p store.Pagination) store.Page[T] {
	total := len(items)
	offset := max(p.Offset, 0)
	if offset >= total {
		return store.Page[T]{
			Items:  []T{},
			Total:  total,
			Limit:  p.Limit,
			Offset: offset,
		}
	}

	end := min(offset+p.Limit, total)

	return store.Page[T]{
		Items:  items[offset:end],
		Total:  total,
		Limit:  p.Limit,
		Offset: offset,
	}
}

// copyMap returns a shallow copy of a map.
func copyMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return nil
	}
	result := make(map[K]V, len(m))
	maps.Copy(result, m)
	return result
}

// copySlice returns a copy of a slice.
func copySlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	result := make([]T, len(s))
	copy(result, s)
	return result
}
