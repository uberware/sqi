// SPDX-License-Identifier: AGPL-3.0-or-later
import type { ProductParameter } from '@/api/types'
import { isRequired, selectWidget } from './productForm'

/** Validate one parameter value; returns an error message or null. HIDDEN
 * params are never validated here (omitted from the form; server fills them). */
export function validateParam(p: ProductParameter, value: string): string | null {
  if (selectWidget(p) === 'hidden') return null

  if (value === '') {
    return isRequired(p) ? `${p.user_interface?.label || p.name} is required` : null
  }

  if (p.allowed_values && p.allowed_values.length > 0 && !p.allowed_values.includes(value)) {
    return `must be one of: ${p.allowed_values.join(', ')}`
  }

  if (p.type === 'INT' || p.type === 'FLOAT') {
    const n = Number(value)
    if (Number.isNaN(n) || (p.type === 'INT' && !Number.isInteger(n))) {
      return `must be a ${p.type === 'INT' ? 'whole ' : ''}number`
    }
    if (p.min_value !== null && n < Number(p.min_value)) return `must be at least ${p.min_value}`
    if (p.max_value !== null && n > Number(p.max_value)) return `must be at most ${p.max_value}`
    return null
  }

  // STRING / PATH length constraints.
  if (p.min_length !== null && value.length < p.min_length)
    return `must be at least ${p.min_length} characters`
  if (p.max_length !== null && value.length > p.max_length)
    return `must be at most ${p.max_length} characters`
  return null
}

/** Validate every parameter; returns a map of name → error for failures only. */
export function validateAll(
  params: ProductParameter[],
  values: Record<string, string>,
): Record<string, string> {
  const errors: Record<string, string> = {}
  for (const p of params) {
    const err = validateParam(p, values[p.name] ?? '')
    if (err) errors[p.name] = err
  }
  return errors
}
