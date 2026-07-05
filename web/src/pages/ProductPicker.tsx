// SPDX-License-Identifier: AGPL-3.0-or-later
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import DebouncedSearchInput from '@/components/DebouncedSearchInput'
import { useSearchParam } from '@/hooks/useSearchParam'
import { filterBySearch } from '@/utils/filterBySearch'
import { useProducts } from '@/api/queries'
import type { Product, ProductSource } from '@/api/types'
import styles from './ProductPicker.module.css'

// Display order + labels for the product source groups. Groups with no
// products are not rendered.
const GROUPS: { source: ProductSource; label: string }[] = [
  { source: 'builtin', label: 'Built In' },
  { source: 'custom', label: 'Custom' },
  { source: 'installed', label: 'Installed' },
]

function ProductCard({ product }: { product: Product }) {
  return (
    <Link className={styles.card} to={`/submit/product/${encodeURIComponent(product.name)}`}>
      <strong>{product.title || product.name}</strong>
      {product.description && <p>{product.description}</p>}
    </Link>
  )
}

export default function ProductPicker() {
  const { data: products, isLoading, error } = useProducts()
  const { search, setSearch } = useSearchParam()

  const filtered = filterBySearch(products ?? [], search, (p) => [p.name, p.title, p.description])

  return (
    <div className={styles.page}>
      <PageHeader title="Submit a job" />
      <div className={styles.content}>
        {isLoading && <p>Loading products…</p>}
        {error && <p role="alert">Failed to load products.</p>}
        {products && products.length === 0 && <p>No products available.</p>}
        {products && products.length > 0 && (
          <DebouncedSearchInput
            value={search}
            onChange={setSearch}
            placeholder="Search products…"
            aria-label="Search products"
            {...(styles.search !== undefined ? { className: styles.search } : {})}
          />
        )}
        {products && products.length > 0 && filtered.length === 0 && (
          <p>No products match “{search}”.</p>
        )}
        {GROUPS.map(({ source, label }) => {
          const inGroup = filtered.filter((p) => p.source === source)
          if (inGroup.length === 0) return null
          return (
            <section className={styles.group} key={source}>
              <div className={styles.groupHeading}>
                <h2 className={styles.groupLabel}>{label}</h2>
                <hr className={styles.rule} />
              </div>
              <div className={styles.grid}>
                {inGroup.map((p) => (
                  <ProductCard key={p.name} product={p} />
                ))}
              </div>
            </section>
          )
        })}
        <p className={styles.advanced}>
          <Link to="/submit/raw">Advanced: submit a raw OpenJD template →</Link>
        </p>
      </div>
    </div>
  )
}
