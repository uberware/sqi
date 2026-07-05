// SPDX-License-Identifier: AGPL-3.0-or-later

package product

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// ErrReadOnly is returned when a mutation targets a read-only product (built-in or installed preset).
var ErrReadOnly = errors.New("product: read-only")

// Catalog overlays the embedded built-ins on a ProductStore and enforces the
// read-only / name-shadowing rules. It is the only component aware of built-ins.
type Catalog struct {
	store store.ProductStore
}

// NewCatalog returns a Catalog backed by st.
func NewCatalog(st store.ProductStore) *Catalog {
	return &Catalog{store: st}
}

// lookupBuiltin performs a case-insensitive lookup in builtinByName (whose keys
// are always lowercase). This ensures "Script" and "SCRIPT" match "script".
func lookupBuiltin(name string) (store.Product, bool) {
	b, ok := builtinByName[strings.ToLower(name)]
	return b, ok
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
	if b, ok := lookupBuiltin(name); ok {
		return b, nil
	}
	return c.store.GetProductByName(ctx, name)
}

// Create stores a new custom/installed product. It rejects names that shadow a
// built-in. An empty Source defaults to SourceCustom; an empty ID is assigned.
func (c *Catalog) Create(ctx context.Context, p store.Product) (store.Product, error) {
	if _, ok := lookupBuiltin(p.Name); ok {
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

// Update replaces a stored product. Built-ins and installed presets are
// read-only (edit an installed preset by duplicating it to a custom product).
func (c *Catalog) Update(ctx context.Context, p store.Product) (store.Product, error) {
	if _, ok := lookupBuiltin(p.Name); ok {
		return store.Product{}, ErrReadOnly
	}
	existing, err := c.store.GetProductByName(ctx, p.Name)
	if err != nil {
		return store.Product{}, err
	}
	if existing.Source == store.SourceInstalled {
		return store.Product{}, ErrReadOnly
	}
	return c.store.UpdateProduct(ctx, p)
}

// Install stores a preset fetched from the library as an installed product.
// It create-or-overwrites by name: a new name is created; an existing installed
// product of the same name is overwritten (the update path); a name shadowing a
// built-in or colliding with a custom product returns store.ErrConflict. The
// returned bool is true when a new product was created, false on overwrite.
func (c *Catalog) Install(ctx context.Context, def store.Product, ref, fingerprint string) (store.Product, bool, error) {
	if _, ok := lookupBuiltin(def.Name); ok {
		return store.Product{}, false, store.ErrConflict
	}
	def.Source = store.SourceInstalled
	def.OriginRef = ref
	def.OriginFingerprint = fingerprint

	existing, err := c.store.GetProductByName(ctx, def.Name)
	if errors.Is(err, store.ErrNotFound) {
		if def.ID == "" {
			def.ID = uuid.NewString()
		}
		created, cerr := c.store.CreateProduct(ctx, def)
		return created, true, cerr
	}
	if err != nil {
		return store.Product{}, false, err
	}
	if existing.Source != store.SourceInstalled {
		return store.Product{}, false, store.ErrConflict
	}
	updated, uerr := c.store.UpdateProduct(ctx, def)
	return updated, false, uerr
}

// Delete removes a stored product. Built-ins are read-only.
func (c *Catalog) Delete(ctx context.Context, name string) error {
	if _, ok := lookupBuiltin(name); ok {
		return ErrReadOnly
	}
	return c.store.DeleteProduct(ctx, name)
}
