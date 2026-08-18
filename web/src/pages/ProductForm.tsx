// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'react'
import { Link, useLocation, useNavigate, useParams } from 'react-router'
import PageHeader from '@/components/PageHeader'
import CodeEditor from '@/components/CodeEditor'
import { useToast } from '@/components/Toast'
import { useProduct } from '@/api/queries'
import { useCreateProduct, useUpdateProduct } from '@/api/mutations'
import type { ProductInput } from '@/api/mutations'
import { ApiError } from '@/api/client'
import { detectFormat } from '@/lib/format'
import { parsePathDeliveries, serializePathDeliveries } from '@/lib/pathDelivery'
import { PRODUCT_LIMITS } from '@/lib/productLimits'
import type { PathTranslation } from '@/api/types'
import { ProductPathDelivery } from '@/pages/ProductPathDelivery'
import styles from './ProductForm.module.css'

interface Props {
  mode: 'create' | 'edit'
}

interface Defaults {
  name: string
  title: string
  description: string
  readme: string
  category: string
  version: string
  template: string
}

/** Router-state payload carried by ProductDetail's "Duplicate to custom". */
export interface ProductDuplicateState {
  duplicateFrom: Defaults
}

// A1 slug: lowercase letters/digits/_/- with one optional "/" segment.
const NAME_PATTERN = /^[a-z0-9][a-z0-9_-]*(\/[a-z0-9][a-z0-9_-]*)?$/

const EMPTY_DEFAULTS: Defaults = {
  name: '',
  title: '',
  description: '',
  readme: '',
  category: '',
  version: '',
  template: '',
}

interface InnerProps {
  mode: 'create' | 'edit'
  defaults: Defaults
}

function ProductFormInner({ mode, defaults }: InnerProps) {
  const navigate = useNavigate()
  const { showToast } = useToast()
  const createProduct = useCreateProduct()
  const updateProduct = useUpdateProduct()

  const [name, setName] = useState(defaults.name)
  const [title, setTitle] = useState(defaults.title)
  const [description, setDescription] = useState(defaults.description)
  const [readme, setReadme] = useState(defaults.readme)
  const [category, setCategory] = useState(defaults.category)
  const [version, setVersion] = useState(defaults.version)
  const [template, setTemplate] = useState(defaults.template)
  const [nameFocused, setNameFocused] = useState(false)

  const mutation = mode === 'create' ? createProduct : updateProduct
  const isPending = createProduct.isPending || updateProduct.isPending

  const format = detectFormat(template)

  let pathTranslation: PathTranslation | null = null
  try {
    pathTranslation = parsePathDeliveries(template, format)
  } catch {
    // invalid template text — panel shows nothing checked
  }

  function handlePathDeliveryChange(pt: PathTranslation) {
    try {
      setTemplate(serializePathDeliveries(template, format, pt))
    } catch {
      // invalid template text — skip update
    }
  }

  const trimmedName = name.trim()
  const nameValid = NAME_PATTERN.test(trimmedName)
  const nameInvalid = trimmedName !== '' && !nameValid
  const canSubmit = nameValid && title.trim() !== '' && template.trim() !== '' && !isPending

  const nameDescribedBy =
    [nameFocused ? 'pf-name-help' : null, nameInvalid ? 'pf-name-error' : null]
      .filter(Boolean)
      .join(' ') || undefined

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    const input: ProductInput = {
      name: trimmedName,
      title: title.trim(),
      description: description.trim(),
      readme: readme.trim(),
      category: category.trim(),
      version: version.trim(),
      template,
      format: detectFormat(template),
    }
    try {
      if (mode === 'create') {
        await createProduct.mutateAsync(input)
        showToast(`Product "${input.name}" created`, 'success')
      } else {
        await updateProduct.mutateAsync({ name: defaults.name, input })
        showToast(`Product "${input.name}" saved`, 'success')
      }
      navigate(`/products/${encodeURIComponent(input.name)}`)
    } catch {
      // Surfaced via the error block below.
    }
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={mode === 'create' ? 'New Product' : 'Edit Product'}
        subtitle="A named, reusable OpenJD template submitted by parameters"
      />

      <form className={styles.form} onSubmit={(e) => void handleSubmit(e)} noValidate>
        <div className={styles.field}>
          <label htmlFor="pf-name" className={styles.label}>
            Name
          </label>
          <div className={styles.nameControl}>
            <input
              id="pf-name"
              className={styles.input}
              value={name}
              onChange={(e) => setName(e.target.value)}
              onFocus={() => setNameFocused(true)}
              onBlur={() => setNameFocused(false)}
              aria-describedby={nameDescribedBy}
              aria-invalid={nameInvalid || undefined}
              required
              aria-required="true"
              maxLength={PRODUCT_LIMITS.name}
            />
            {(nameFocused || nameInvalid) && (
              <div className={styles.nameHelp}>
                {nameFocused && (
                  <p id="pf-name-help" className={styles.hint}>
                    Stable identity, e.g. &quot;maya-render&quot; or &quot;studio/maya-render&quot;.
                    Lowercase letters, digits, dashes, underscores, and one optional &quot;/&quot;
                    segment.
                  </p>
                )}
                {nameInvalid && (
                  <p id="pf-name-error" className={styles.hint} role="alert">
                    Name must be a lowercase slug, optionally with one &quot;/&quot; segment.
                  </p>
                )}
              </div>
            )}
          </div>
        </div>

        <div className={styles.field}>
          <label htmlFor="pf-title" className={styles.label}>
            Title
          </label>
          <input
            id="pf-title"
            className={styles.input}
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            required
            aria-required="true"
            maxLength={PRODUCT_LIMITS.title}
          />
        </div>

        <div className={styles.field}>
          <label htmlFor="pf-description" className={styles.label}>
            Description (optional)
          </label>
          <input
            id="pf-description"
            className={styles.input}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            maxLength={PRODUCT_LIMITS.description}
          />
        </div>

        <div className={styles.field}>
          <label htmlFor="pf-readme" className={styles.label}>
            Readme (optional, Markdown)
          </label>
          <textarea
            id="pf-readme"
            className={styles.textarea}
            rows={10}
            maxLength={PRODUCT_LIMITS.readme}
            value={readme}
            onChange={(e) => setReadme(e.target.value)}
            aria-describedby="pf-readme-help"
          />
          <p id="pf-readme-help" className={styles.hint}>
            Long-form documentation, shown on this product&apos;s detail page. Supports paragraphs,
            lists, fenced code, headings, bold, italic, inline code and links.
          </p>
        </div>

        <div className={styles.row}>
          <div className={styles.field}>
            <label htmlFor="pf-category" className={styles.label}>
              Category (optional)
            </label>
            <input
              id="pf-category"
              className={styles.input}
              value={category}
              onChange={(e) => setCategory(e.target.value)}
              maxLength={PRODUCT_LIMITS.category}
            />
          </div>
          <div className={styles.field}>
            <label htmlFor="pf-version" className={styles.label}>
              Version (optional)
            </label>
            <input
              id="pf-version"
              className={styles.input}
              value={version}
              onChange={(e) => setVersion(e.target.value)}
              maxLength={PRODUCT_LIMITS.version}
            />
          </div>
        </div>

        <div className={styles.field}>
          <span className={styles.label}>OpenJD Template</span>
          <div className={styles.editorWrap}>
            <CodeEditor
              value={template}
              onChange={setTemplate}
              aria-label="OpenJD template"
              data-testid="template-editor"
            />
          </div>
        </div>

        <ProductPathDelivery value={pathTranslation} onChange={handlePathDeliveryChange} />

        {mutation.isError && (
          <div className={styles.errorBlock} role="alert">
            <strong className={styles.errorTitle}>Save failed</strong>
            <pre className={styles.errorDetail}>
              {mutation.error instanceof ApiError ? mutation.error.detail : String(mutation.error)}
            </pre>
          </div>
        )}

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={!canSubmit}>
            {mode === 'create' ? 'Create Product' : 'Save'}
          </button>
          <Link to="/products" className={styles.cancelBtn}>
            Cancel
          </Link>
        </div>
      </form>
    </div>
  )
}

export default function ProductForm({ mode }: Props) {
  const params = useParams<{ name: string }>()
  const location = useLocation()
  const name = params.name ?? ''
  const { data, isLoading, isError } = useProduct(mode === 'edit' ? name : '')

  if (mode === 'edit') {
    if (isLoading || !data) {
      return (
        <div className={styles.page}>
          {isError ? <p>Failed to load product.</p> : <p>Loading…</p>}
        </div>
      )
    }
    return (
      <ProductFormInner
        key={data.name}
        mode="edit"
        defaults={{
          name: data.name,
          title: data.title,
          description: data.description,
          readme: data.readme,
          category: data.category,
          version: data.version,
          template: data.template,
        }}
      />
    )
  }

  const state = location.state as ProductDuplicateState | null
  const defaults = state?.duplicateFrom ?? EMPTY_DEFAULTS
  return <ProductFormInner mode="create" defaults={defaults} />
}
