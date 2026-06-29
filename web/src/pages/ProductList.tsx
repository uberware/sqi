// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import IconButton from '@/components/IconButton'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import { useProducts } from '@/api/queries'
import { useDeleteProduct } from '@/api/mutations'
import styles from './ProductList.module.css'

export default function ProductList() {
  const { data: products, isLoading, isError, error } = useProducts()
  const deleteProduct = useDeleteProduct()
  const { showToast } = useToast()
  const [deletingNames, setDeletingNames] = useState<Set<string>>(new Set())

  const handleDelete = useCallback(
    async (name: string) => {
      if (!window.confirm(`Delete product "${name}"? This cannot be undone.`)) return
      setDeletingNames((s) => new Set(s).add(name))
      try {
        await deleteProduct.mutateAsync(name)
        showToast(`Product "${name}" deleted`, 'success')
      } catch (e) {
        showToast(e instanceof Error ? e.message : 'Failed to delete product', 'error')
      } finally {
        setDeletingNames((s) => {
          const next = new Set(s)
          next.delete(name)
          return next
        })
      }
    },
    [deleteProduct, showToast],
  )

  const rows = products ?? []

  return (
    <div className={styles.page}>
      <PageHeader
        title="Products"
        backTo="/admin"
        subtitle={isLoading ? 'Loading…' : `${rows.length} products`}
        action={
          <Link to="/products/new" className={styles.newBtn}>
            + New Product
          </Link>
        }
      />

      {isError && (
        <div className={styles.errorBanner} role="alert">
          Failed to load products: {error instanceof Error ? error.message : 'Unknown error'}
        </div>
      )}

      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Products">
          <thead>
            <tr>
              <th>Name</th>
              <th>Title</th>
              <th>Category</th>
              <th>Version</th>
              <th>Source</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={styles.emptyRow}>
                <td colSpan={6}>Loading…</td>
              </tr>
            )}
            {!isLoading && rows.length === 0 && (
              <tr className={styles.emptyRow}>
                <td colSpan={6}>
                  No products yet. Products are reusable, named OpenJD recipes submitted by
                  parameters.{' '}
                  <a
                    href="https://github.com/uberware/sqi/blob/main/docs/products.md"
                    target="_blank"
                    rel="noreferrer"
                  >
                    Learn more
                  </a>
                  .
                </td>
              </tr>
            )}
            {rows.map((product) => (
              <tr key={product.name}>
                <td>
                  <Link
                    to={`/products/${encodeURIComponent(product.name)}`}
                    className={styles.linkBtn}
                  >
                    {product.name}
                  </Link>
                </td>
                <td>{product.title}</td>
                <td>{product.category || '—'}</td>
                <td>{product.version || '—'}</td>
                <td>
                  <span className={styles.badge} data-source={product.source}>
                    {product.source}
                  </span>
                </td>
                <td>
                  {product.source !== 'builtin' && (
                    <IconButton
                      icon={<Trash />}
                      className={styles.deleteBtn}
                      busy={deletingNames.has(product.name)}
                      onClick={() => void handleDelete(product.name)}
                      title="Delete"
                      label={`Delete product ${product.name}`}
                    />
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
