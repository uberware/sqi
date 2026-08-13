// SPDX-License-Identifier: AGPL-3.0-or-later
import { useId } from 'react'
import type { ProductParameter } from '@/api/types'
import { selectWidget, paramLabel, isRequired } from '@/lib/productForm'
import ListParamField from '@/components/ListParamField'
import styles from './ProductParamField.module.css'

interface Props {
  param: ProductParameter
  value: string
  error?: string
  onChange: (value: string) => void
}

export default function ProductParamField({ param, value, error, onChange }: Props) {
  const id = useId()
  const widget = selectWidget(param)
  if (widget === 'hidden') return null

  const label = paramLabel(param)
  const required = isRequired(param)

  function renderControl() {
    switch (widget) {
      case 'select':
        return (
          <select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
            {(param.allowed_values ?? []).map((opt) => (
              <option key={opt} value={opt}>
                {opt}
              </option>
            ))}
          </select>
        )
      case 'checkbox': {
        const [off, on] = param.allowed_values ?? ['false', 'true']
        return (
          <input
            id={id}
            type="checkbox"
            checked={value === on}
            onChange={(e) => onChange(e.target.checked ? (on ?? 'true') : (off ?? 'false'))}
          />
        )
      }
      case 'textarea':
        return <textarea id={id} value={value} onChange={(e) => onChange(e.target.value)} />
      case 'number':
        return (
          <input id={id} type="number" value={value} onChange={(e) => onChange(e.target.value)} />
        )
      case 'list':
        return <ListParamField param={param} value={value} onChange={onChange} />
      default:
        return (
          <input id={id} type="text" value={value} onChange={(e) => onChange(e.target.value)} />
        )
    }
  }

  return (
    <div className={styles.field}>
      <label className={styles.label} htmlFor={id}>
        {label}
        {required && (
          <span className={styles.required} aria-hidden="true">
            *
          </span>
        )}
      </label>
      {param.description && <span>{param.description}</span>}
      {renderControl()}
      {error && (
        <span className={styles.error} role="alert">
          {error}
        </span>
      )}
    </div>
  )
}
