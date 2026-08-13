// SPDX-License-Identifier: AGPL-3.0-or-later
import { useId } from 'react'
import type { ProductParameter } from '@/api/types'
import { listElementType } from '@/lib/productForm'
import styles from './ListParamField.module.css'

interface Props {
  param: ProductParameter
  value: string
  onChange: (value: string) => void
}

/** A JSON element as it round-trips through this editor. Numbers and booleans
 * keep their JSON types so the encoding matches the server's canonical form
 * (internal/openjd/paramjson.go); a string element that is mid-typing — "-" in
 * a number field — stays a string until it parses. */
type Element = string | number | boolean

/** Decode the incoming value, or null when it is not a JSON array. Null means
 * "render a raw text field": a stored default must never break the form. */
function decode(value: string): Element[] | null {
  if (value.trim() === '') return []
  let parsed: unknown
  try {
    parsed = JSON.parse(value)
  } catch {
    return null
  }
  if (!Array.isArray(parsed)) return null
  return parsed.map((e): Element => {
    if (typeof e === 'number' || typeof e === 'boolean' || typeof e === 'string') return e
    return String(e)
  })
}

/** Serialise to the server's canonical form: JSON.stringify of an array
 * produces no spaces and each element as its JSON type, which is exactly what
 * internal/openjd/paramjson.go's marshalCanonical emits. */
function encode(elements: Element[]): string {
  return JSON.stringify(elements)
}

/** Interpret a row's raw input for the declared element type. A value that is
 * not yet a number stays a string rather than being coerced or dropped. */
function toElement(raw: string, elementType: string | null): Element {
  if (elementType === 'INT' || elementType === 'FLOAT') {
    if (raw.trim() === '') return ''
    const n = Number(raw)
    return Number.isNaN(n) ? raw : n
  }
  return raw
}

export default function ListParamField({ param, value, onChange }: Props) {
  const id = useId()
  const elements = decode(value)
  const elementType = listElementType(param.type)

  if (elements === null) {
    return (
      <input
        id={id}
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        aria-label={`${param.name} (raw JSON)`}
      />
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
    <div className={styles.list}>
      {elements.map((element, index) => (
        // The index IS the identity here: elements are positional and have no
        // stable key of their own.
        <div className={styles.row} key={index}>
          {elementType === 'BOOL' ? (
            <input
              type="checkbox"
              checked={element === true}
              aria-label={`${param.name} item ${index + 1}`}
              onChange={(e) => replace(index, e.target.checked)}
            />
          ) : (
            <input
              // A native type="number" input sanitizes its DOM value to "" for
              // an in-progress entry like "-" or "1e" (the HTML value
              // sanitization algorithm applies even to a value set
              // programmatically), so a controlled component can never read
              // back what was actually typed. text + inputMode keeps the
              // numeric keyboard on mobile without losing the draft; the
              // explicit role keeps the same accessible semantics a
              // type="number" input would have had.
              type="text"
              inputMode={elementType === 'INT' || elementType === 'FLOAT' ? 'decimal' : 'text'}
              role={elementType === 'INT' || elementType === 'FLOAT' ? 'spinbutton' : undefined}
              value={String(element)}
              aria-label={`${param.name} item ${index + 1}`}
              onChange={(e) => replace(index, toElement(e.target.value, elementType))}
            />
          )}
          <button type="button" onClick={() => remove(index)}>
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
