// SPDX-License-Identifier: AGPL-3.0-or-later
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import { useProducts } from '@/api/queries'
import styles from './ProductPicker.module.css'

export default function ProductPicker() {
  const { data: products, isLoading, error } = useProducts()

  return (
    <div>
      <PageHeader title="Submit a job" />
      {isLoading && <p>Loading products…</p>}
      {error && <p role="alert">Failed to load products.</p>}
      {products && products.length === 0 && <p>No products available.</p>}
      {products && products.length > 0 && (
        <div className={styles.grid}>
          {products.map((p) => (
            <Link
              key={p.name}
              className={styles.card}
              to={`/submit/product/${encodeURIComponent(p.name)}`}
            >
              <strong>{p.title || p.name}</strong>
              {p.description && <p>{p.description}</p>}
            </Link>
          ))}
        </div>
      )}
      <p className={styles.advanced}>
        <Link to="/submit/raw">Advanced: submit a raw OpenJD template →</Link>
      </p>
    </div>
  )
}
