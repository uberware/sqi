// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"

	"github.com/uberware/sqi/internal/store"
)

// CreateProduct inserts a product. Returns [store.ErrConflict] on duplicate name.
func (s *Store) CreateProduct(_ context.Context, p store.Product) (store.Product, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.products {
		if existing.Name == p.Name {
			return store.Product{}, store.ErrConflict
		}
	}
	s.products[p.ID] = p
	return p, nil
}

// GetProductByName returns the product with the given name, or [store.ErrNotFound].
func (s *Store) GetProductByName(_ context.Context, name string) (store.Product, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.products {
		if p.Name == name {
			return p, nil
		}
	}
	return store.Product{}, store.ErrNotFound
}

// ListProducts returns all products ordered by name.
func (s *Store) ListProducts(_ context.Context) ([]store.Product, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Product, 0, len(s.products))
	for _, p := range s.products {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b store.Product) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return out, nil
}

// UpdateProduct replaces mutable fields of the product with p.Name.
func (s *Store) UpdateProduct(_ context.Context, p store.Product) (store.Product, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, existing := range s.products {
		if existing.Name == p.Name {
			p.ID = existing.ID
			p.CreatedAt = existing.CreatedAt
			s.products[id] = p
			return p, nil
		}
	}
	return store.Product{}, store.ErrNotFound
}

// DeleteProduct removes the product with the given name.
func (s *Store) DeleteProduct(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.products {
		if p.Name == name {
			delete(s.products, id)
			return nil
		}
	}
	return store.ErrNotFound
}
