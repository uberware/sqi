// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	sqlInsertProduct = `
INSERT INTO products (id, name, title, description, category, version, source, template, format, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, name, title, description, category, version, source, template, format, created_at, updated_at`

	sqlGetProductByName = `
SELECT id, name, title, description, category, version, source, template, format, created_at, updated_at
FROM products WHERE name = ?`

	sqlListProducts = `
SELECT id, name, title, description, category, version, source, template, format, created_at, updated_at
FROM products ORDER BY name`

	sqlUpdateProduct = `
UPDATE products
SET title = ?, description = ?, category = ?, version = ?, source = ?, template = ?, format = ?, updated_at = ?
WHERE name = ?
RETURNING id, name, title, description, category, version, source, template, format, created_at, updated_at`

	sqlDeleteProduct = `DELETE FROM products WHERE name = ?`
)

func scanProduct(row scanner) (store.Product, error) {
	var p store.Product
	var source, format, createdAt, updatedAt string
	if err := row.Scan(
		&p.ID, &p.Name, &p.Title, &p.Description, &p.Category, &p.Version,
		&source, &p.Template, &format, &createdAt, &updatedAt,
	); err != nil {
		return store.Product{}, err
	}
	p.Source = store.Source(source)
	p.Format = store.TemplateFormat(format)
	p.CreatedAt = mustTime(createdAt)
	p.UpdatedAt = mustTime(updatedAt)
	return p, nil
}

// CreateProduct implements [store.ProductStore].
func (s *Store) CreateProduct(ctx context.Context, p store.Product) (store.Product, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertProduct.QueryRowContext(ctx,
		p.ID, p.Name, p.Title, p.Description, p.Category, p.Version,
		string(p.Source), p.Template, string(p.Format), now, now)
	out, err := scanProduct(row)
	return out, mapErr(err)
}

// GetProductByName implements [store.ProductStore].
func (s *Store) GetProductByName(ctx context.Context, name string) (store.Product, error) {
	row := s.stmtGetProductByName.QueryRowContext(ctx, name)
	out, err := scanProduct(row)
	return out, mapErr(err)
}

// ListProducts implements [store.ProductStore].
func (s *Store) ListProducts(ctx context.Context) ([]store.Product, error) {
	rows, err := s.stmtListProducts.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Product
	for rows.Next() {
		p, scanErr := scanProduct(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, p)
	}
	return out, mapErr(rows.Err())
}

// UpdateProduct implements [store.ProductStore].
func (s *Store) UpdateProduct(ctx context.Context, p store.Product) (store.Product, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateProduct.QueryRowContext(ctx,
		p.Title, p.Description, p.Category, p.Version, string(p.Source),
		p.Template, string(p.Format), now, p.Name)
	out, err := scanProduct(row)
	return out, mapErr(err)
}

// DeleteProduct implements [store.ProductStore].
func (s *Store) DeleteProduct(ctx context.Context, name string) error {
	res, err := s.stmtDeleteProduct.ExecContext(ctx, name)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
