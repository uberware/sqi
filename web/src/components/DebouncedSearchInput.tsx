// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useEffect, useRef } from 'react'
import SearchInput from './SearchInput'
import { useDebounce } from '@/hooks/useDebounce'

export interface DebouncedSearchInputProps {
  value: string
  onChange: (value: string) => void
  placeholder: string
  'aria-label': string
  className?: string
}

/**
 * SearchInput that debounces edits (300ms) before emitting `onChange`.
 * `value` seeds the input on mount (e.g. from a URL param); the initial
 * mount does not emit, so an existing URL search value isn't re-emitted.
 */
export default function DebouncedSearchInput({
  value,
  onChange,
  placeholder,
  'aria-label': ariaLabel,
  className,
}: DebouncedSearchInputProps) {
  const [input, setInput] = useState(value)
  const debounced = useDebounce(input, 300)

  const didMount = useRef(false)
  useEffect(() => {
    if (!didMount.current) {
      didMount.current = true
      return
    }
    onChange(debounced)
    // onChange is a stable callback from the parent's filter hook.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debounced])

  return (
    <SearchInput
      value={input}
      onChange={setInput}
      placeholder={placeholder}
      aria-label={ariaLabel}
      {...(className !== undefined ? { className } : {})}
    />
  )
}
