// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import { useProduct } from '@/api/queries'
import { useDeleteProduct } from '@/api/mutations'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import type { ProductDuplicateState } from './ProductForm'
import styles from './ProductDetail.module.css'

export default function ProductDetail() {
  const params = useParams<{ name: string }>()
  const name = params.name ?? ''
  const navigate = useNavigate()
  const { showToast } = useToast()
  const { principal } = useAuth()
  const canManage = can(principal, 'products.manage')
  const { data: product, isLoading, isError } = useProduct(name)
  const deleteProduct = useDeleteProduct()

  const handleDelete = useCallback(async () => {
    if (!product) return
    if (!window.confirm(`Delete product "${product.name}"? This cannot be undone.`)) return
    try {
      await deleteProduct.mutateAsync(product.name)
      showToast(`Product "${product.name}" deleted`, 'success')
      navigate('/products')
    } catch (e) {
      showToast(e instanceof Error ? e.message : 'Failed to delete product', 'error')
    }
  }, [product, deleteProduct, showToast, navigate])

  const handleDuplicate = useCallback(() => {
    if (!product) return
    const state: ProductDuplicateState = {
      duplicateFrom: {
        name: '',
        title: product.title,
        description: product.description,
        category: product.category,
        version: product.version,
        template: product.template,
      },
    }
    navigate('/products/new', { state })
  }, [product, navigate])

  if (isLoading || !product) {
    return (
      <div className={styles.page}>
        {isError ? <p>Failed to load product.</p> : <p>Loading…</p>}
      </div>
    )
  }

  const isInstalled = product.source === 'installed'
  const canEdit = canManage && product.source === 'custom'
  const canDelete = canManage && (product.source === 'custom' || isInstalled)

  return (
    <div className={styles.page}>
      <PageHeader
        title="Product Details"
        backTo="/products"
        backLabel="Products"
        action={
          <div className={styles.actions}>
            {canManage && (
              <button type="button" className={styles.actionBtn} onClick={handleDuplicate}>
                Duplicate to custom
              </button>
            )}
            {canEdit && (
              <Link
                to={`/products/${encodeURIComponent(product.name)}/edit`}
                className={styles.actionBtn}
              >
                Edit
              </Link>
            )}
            {canDelete && (
              <button
                type="button"
                className={styles.deleteBtn}
                onClick={() => void handleDelete()}
                disabled={deleteProduct.isPending}
              >
                {isInstalled ? 'Uninstall' : 'Delete'}
              </button>
            )}
          </div>
        }
      />

      <div className={styles.nameArea}>
        <h2 className={styles.productName}>{product.title}</h2>
        {product.description && <p className={styles.productDescription}>{product.description}</p>}
      </div>

      <dl className={styles.meta}>
        <dt>Name</dt>
        <dd>{product.name}</dd>
        <dt>Category</dt>
        <dd>{product.category || '—'}</dd>
        <dt>Version</dt>
        <dd>{product.version || '—'}</dd>
        <dt>Source</dt>
        <dd>
          <span className={styles.badge} data-source={product.source}>
            {product.source}
          </span>
        </dd>
      </dl>

      <h2 className={styles.sectionTitle}>OpenJD Template</h2>
      <pre className={styles.template} aria-label="OpenJD template">
        {product.template}
      </pre>
    </div>
  )
}
