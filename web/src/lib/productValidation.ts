// SPDX-License-Identifier: AGPL-3.0-or-later
import type { ItemConstraint, ProductParameter } from '@/api/types'
import { isRequired, listElementType, selectWidget } from './productForm'

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

  const elementType = listElementType(p.type)
  if (elementType !== null) {
    return validateListValue(p, value, elementType)
  }

  // STRING / PATH length constraints. RANGE_EXPR's grammar and BOOL's
  // accepted spellings are the server's to enforce (see validateListValue's
  // doc comment for why a second copy would be a drift hazard); RANGE_EXPR's
  // min_length/max_length bound its string length exactly as the server
  // does, and BOOL declares neither, so both correctly fall through here.
  if (p.min_length !== null && value.length < p.min_length)
    return `must be at least ${p.min_length} characters`
  if (p.max_length !== null && value.length > p.max_length)
    return `must be at most ${p.max_length} characters`
  return null
}

/** Validate a LIST[*] value: it must be a JSON array, of an allowed length,
 * whose every element satisfies its declared type and the item: constraints.
 *
 * Errors name the offending row ("item 3"), which is why the editor renders
 * one input per element rather than a single textarea. */
function validateListValue(
  p: ProductParameter,
  value: string,
  elementType: ProductParameter['type'],
): string | null {
  let parsed: unknown
  try {
    parsed = JSON.parse(value)
  } catch {
    return 'must be a list'
  }
  if (!Array.isArray(parsed)) return 'must be a list'

  if (p.min_length !== null && parsed.length < p.min_length)
    return `must have at least ${p.min_length} items`
  if (p.max_length !== null && parsed.length > p.max_length)
    return `must have at most ${p.max_length} items`

  for (let i = 0; i < parsed.length; i++) {
    const err = validateElement(parsed[i], elementType, p.item)
    if (err) return `item ${i + 1} ${err}`
  }
  return null
}

/** Validate one element against its declared type and the item: constraints
 * for its level. Deliberately shallow for BOOL — the server owns the accepted
 * spellings — and for an inner list, whose own elements the raw-JSON fallback
 * leaves to the server. */
function validateElement(
  element: unknown,
  elementType: ProductParameter['type'],
  item: ItemConstraint | null,
): string | null {
  if (elementType === 'INT' || elementType === 'FLOAT') {
    if (typeof element !== 'number') return `must be a number`
    if (elementType === 'INT' && !Number.isInteger(element)) return `must be a whole number`
    if (item?.min_value != null && element < Number(item.min_value))
      return `must be at least ${item.min_value}`
    if (item?.max_value != null && element > Number(item.max_value))
      return `must be at most ${item.max_value}`
  } else if (elementType === 'STRING' || elementType === 'PATH') {
    if (typeof element !== 'string') return `must be text`
    if (item?.min_length != null && element.length < item.min_length)
      return `must be at least ${item.min_length} characters`
    if (item?.max_length != null && element.length > item.max_length)
      return `must be at most ${item.max_length} characters`
  }

  if (item?.allowed_values && item.allowed_values.length > 0) {
    if (!item.allowed_values.includes(String(element)))
      return `must be one of: ${item.allowed_values.join(', ')}`
  }
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
