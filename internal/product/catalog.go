// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// ErrReadOnly is returned when a mutation targets a built-in product.
var ErrReadOnly = errors.New("product: built-in is read-only")

// Catalog overlays the embedded built-ins on a ProductStore and enforces the
// read-only / name-shadowing rules. It is the only component aware of built-ins.
type Catalog struct {
	store store.ProductStore
}

// NewCatalog returns a Catalog backed by st.
func NewCatalog(st store.ProductStore) *Catalog {
	return &Catalog{store: st}
}

// List returns built-ins merged with stored products, ordered by name.
func (c *Catalog) List(ctx context.Context) ([]store.Product, error) {
	stored, err := c.store.ListProducts(ctx)
	if err != nil {
		return nil, err
	}
	out := append(Builtins(), stored...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetByName returns a built-in (preferred) or stored product, or ErrNotFound.
func (c *Catalog) GetByName(ctx context.Context, name string) (store.Product, error) {
	if b, ok := builtinByName[name]; ok {
		return b, nil
	}
	return c.store.GetProductByName(ctx, name)
}

// Create stores a new custom/installed product. It rejects names that shadow a
// built-in. An empty Source defaults to SourceCustom; an empty ID is assigned.
func (c *Catalog) Create(ctx context.Context, p store.Product) (store.Product, error) {
	if _, ok := builtinByName[p.Name]; ok {
		return store.Product{}, store.ErrConflict
	}
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.Source == "" {
		p.Source = store.SourceCustom
	}
	return c.store.CreateProduct(ctx, p)
}

// Update replaces a stored product. Built-ins are read-only.
func (c *Catalog) Update(ctx context.Context, p store.Product) (store.Product, error) {
	if _, ok := builtinByName[p.Name]; ok {
		return store.Product{}, ErrReadOnly
	}
	return c.store.UpdateProduct(ctx, p)
}

// Delete removes a stored product. Built-ins are read-only.
func (c *Catalog) Delete(ctx context.Context, name string) error {
	if _, ok := builtinByName[name]; ok {
		return ErrReadOnly
	}
	return c.store.DeleteProduct(ctx, name)
}
