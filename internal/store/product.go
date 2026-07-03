// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// Source identifies where a product definition came from.
type Source string

const (
	// SourceBuiltin is a product shipped embedded in the binary (read-only).
	SourceBuiltin Source = "builtin"
	// SourceCustom is a product authored on this server.
	SourceCustom Source = "custom"
	// SourceInstalled is a product installed from the community preset repo.
	SourceInstalled Source = "installed"
)

// Product is a named, versioned wrapper around a verbatim OpenJD template.
// Built-ins (SourceBuiltin) are served from the binary and never stored here;
// the products table holds only SourceCustom and SourceInstalled rows.
type Product struct {
	ID          string
	Name        string // stable identity, e.g. "script", "studio/maya-render"
	Title       string
	Description string
	Category    string
	Version     string
	Source      Source
	Template    string // verbatim OpenJD template
	Format      TemplateFormat
	// OriginRef is the preset-library index entry name this product was
	// installed from; empty for builtin/custom products.
	OriginRef string
	// OriginFingerprint is the definition's sha256 at install time; empty for
	// builtin/custom products. Compared against the index to detect updates.
	OriginFingerprint string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ProductStore persists custom/installed [Product] rows. Built-in overlay and
// read-only enforcement live in internal/product.Catalog, not here.
type ProductStore interface {
	// CreateProduct inserts a product. Returns [ErrConflict] on duplicate name.
	CreateProduct(ctx context.Context, p Product) (Product, error)
	// GetProductByName returns the product with the given name, or [ErrNotFound].
	GetProductByName(ctx context.Context, name string) (Product, error)
	// ListProducts returns all stored products ordered by name.
	ListProducts(ctx context.Context) ([]Product, error)
	// UpdateProduct replaces mutable fields of the product with p.Name.
	// Returns [ErrNotFound] if no such product exists.
	UpdateProduct(ctx context.Context, p Product) (Product, error)
	// DeleteProduct removes the product with the given name. Returns
	// [ErrNotFound] if it does not exist.
	DeleteProduct(ctx context.Context, name string) error
}
