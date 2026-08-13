// SPDX-License-Identifier: AGPL-3.0-or-later
import type { ProductParameter } from '@/api/types'
import { isBoolTruthy, listElementType, paramLabel } from '@/lib/productForm'
import styles from './ListParamField.module.css'

interface Props {
  param: ProductParameter
  /** The id of the field's own <label>, supplied by ProductParamField. Used
   * as the target of aria-labelledby on this widget's role="group" wrapper
   * -- a group is not a labelable element, so htmlFor/id association (as
   * every other widget uses) does not apply here. */
  id: string
  value: string
  onChange: (value: string) => void
}

/** A JSON element as it round-trips through this editor. Numbers and booleans
 * keep their JSON types so the encoding matches the server's canonical form
 * (internal/openjd/paramjson.go); a string element that is mid-typing — "-" in
 * a number field — stays a string until it parses. */
type Element = string | number | boolean

/** Decode the incoming value, or null when it is not a JSON array of scalars.
 * Null means "render a raw text field": a stored default must never break
 * the form.
 *
 * This is NOT a value-preserving round trip in general -- a Go-produced
 * default can never contain a non-scalar (encodeListDefault in
 * internal/openjd/paramjson.go rejects them), but a hand-edited or otherwise
 * malformed stored value could. Rather than laundering a `null`, an object,
 * or a nested array through String() -- which would silently rewrite it, and
 * because every edit re-encodes the whole array, would rewrite an unrelated
 * row on the next edit -- any non-scalar element falls back to the raw-JSON
 * field, same as an unparsable value. */
function decode(value: string): Element[] | null {
  if (value.trim() === '') return []
  let parsed: unknown
  try {
    parsed = JSON.parse(value)
  } catch {
    return null
  }
  if (!Array.isArray(parsed)) return null
  const elements: Element[] = []
  for (const e of parsed) {
    if (typeof e !== 'number' && typeof e !== 'boolean' && typeof e !== 'string') return null
    elements.push(e)
  }
  return elements
}

/** Serialise to the server's canonical form: JSON.stringify of an array
 * produces no spaces and each element as its JSON type, which is exactly what
 * internal/openjd/paramjson.go's marshalCanonical emits. */
function encode(elements: Element[]): string {
  return JSON.stringify(elements)
}

/** Interpret a row's raw input for the declared element type. A value that is
 * not yet a number, or whose text isn't what Number's own canonical string
 * form would produce for it, stays a string rather than being coerced or
 * dropped.
 *
 * The second half of that check is what a plain `Number.isNaN` test misses:
 * `Number("1.")` is `1`, not NaN. Without it, a controlled input showing "1"
 * that the user extends to "1." would re-render back to "1" -- React's
 * controlled-input restore -- eating the "." the user just typed. Keeping
 * "1.", "1.50" and "01" as strings (they fail String(Number(raw)) === raw)
 * means the rendered value always matches what was typed; validateElement
 * reports them as "must be a number" at submit, the same behaviour a
 * mid-typing "-" already has. */
function toElement(raw: string, elementType: string | null): Element {
  if (elementType === 'INT' || elementType === 'FLOAT') {
    const trimmed = raw.trim()
    if (trimmed === '') return ''
    const n = Number(raw)
    return Number.isNaN(n) || String(n) !== trimmed ? raw : n
  }
  return raw
}

export default function ListParamField({ param, id, value, onChange }: Props) {
  const label = paramLabel(param)
  const elements = decode(value)
  const elementType = listElementType(param.type)

  if (elements === null) {
    return (
      <div role="group" aria-labelledby={id}>
        <input
          type="text"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          aria-label={`${label} (raw JSON)`}
        />
      </div>
    )
  }

  function replace(index: number, next: Element) {
    const copy = [...(elements ?? [])]
    copy[index] = next
    onChange(encode(copy))
  }

  function remove(index: number) {
    onChange(encode((elements ?? []).filter((_, i) => i !== index)))
  }

  function add() {
    onChange(encode([...(elements ?? []), elementType === 'BOOL' ? false : '']))
  }

  return (
    <div className={styles.list} role="group" aria-labelledby={id}>
      {elements.map((element, index) => (
        // The index IS the identity here: elements are positional and have no
        // stable key of their own.
        <div className={styles.row} key={index}>
          {elementType === 'BOOL' ? (
            <input
              type="checkbox"
              checked={isBoolTruthy(element)}
              aria-label={`${label} item ${index + 1}`}
              onChange={(e) => replace(index, e.target.checked)}
            />
          ) : (
            <input
              // Deliberately type="text", not type="number": the HTML value
              // sanitization algorithm zeroes a type="number" input's DOM
              // value to "" for any in-progress entry that isn't yet a valid
              // number ("-", "1e"), and it does this on every set — including
              // a controlled component's own re-render — not just on blur. A
              // controlled numeric field built on type="number" can
              // therefore never read back a mid-typing draft; it would
              // silently coerce or drop it. inputMode keeps the numeric
              // keyboard on mobile without that loss. This is a genuine text
              // field (see toElement below), so it keeps the default
              // textbox role rather than overriding to spinbutton, which
              // would need aria-valuenow/min/max and arrow-key stepping to
              // be a correct override — see docs/web-accessibility.md.
              type="text"
              inputMode={elementType === 'INT' || elementType === 'FLOAT' ? 'decimal' : 'text'}
              value={String(element)}
              aria-label={`${label} item ${index + 1}`}
              onChange={(e) => replace(index, toElement(e.target.value, elementType))}
            />
          )}
          <button
            type="button"
            aria-label={`Remove ${label} item ${index + 1}`}
            onClick={() => remove(index)}
          >
            Remove
          </button>
        </div>
      ))}
      <button type="button" onClick={add}>
        Add item
      </button>
    </div>
  )
}
