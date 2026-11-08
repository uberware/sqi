// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useId, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { useSetJobEstimate } from '@/api/mutations'
import { formatDuration, formatDurationInput, parseDurationInput } from '@/lib/time'
import styles from './EstimateField.module.css'

interface EstimateFieldProps {
  jobId: string
  seconds: number | undefined
  /** Whether the viewer may change the estimate. */
  editable: boolean
}

const HINT = 'Use h, m or s, e.g. 1h 30m'

/** A job's run-time estimate, with an inline editor when the viewer may change it. */
export default function EstimateField({ jobId, seconds, editable }: EstimateFieldProps) {
  const [editing, setEditing] = useState(false)
  const [text, setText] = useState('')
  const [invalid, setInvalid] = useState(false)
  const setEstimate = useSetJobEstimate()
  const inputRef = useRef<HTMLInputElement>(null)
  const editRef = useRef<HTMLButtonElement>(null)
  const wasEditing = useRef(false)
  const inputId = useId()
  const hintId = useId()

  useEffect(() => {
    if (editing) inputRef.current?.focus()
    else if (wasEditing.current) editRef.current?.focus()
    wasEditing.current = editing
  }, [editing])

  const display = seconds === undefined ? '—' : formatDuration(seconds * 1000)

  if (!editing) {
    return (
      <span className={styles.row}>
        <span>{display}</span>
        {editable && (
          <button
            ref={editRef}
            type="button"
            className={styles.link}
            aria-label="Edit estimate"
            onClick={() => {
              setText(seconds === undefined ? '' : formatDurationInput(seconds))
              setInvalid(false)
              setEditing(true)
            }}
          >
            Edit
          </button>
        )}
      </span>
    )
  }

  function save(e: FormEvent) {
    e.preventDefault()
    const trimmed = text.trim()
    let value: number | null = null
    if (trimmed !== '') {
      const parsed = parseDurationInput(trimmed)
      if (parsed === undefined) {
        setInvalid(true)
        return
      }
      value = parsed
    }
    setInvalid(false)
    setEstimate.mutate({ id: jobId, seconds: value }, { onSuccess: () => setEditing(false) })
  }

  return (
    <form className={styles.form} onSubmit={save}>
      <label htmlFor={inputId} className={styles.label}>
        Estimated run time
      </label>
      <input
        ref={inputRef}
        id={inputId}
        className={styles.input}
        type="text"
        value={text}
        placeholder="1h 30m"
        aria-invalid={invalid}
        aria-describedby={invalid ? hintId : undefined}
        onChange={(e) => setText(e.target.value)}
      />
      <span className={styles.actions}>
        <button type="submit" className={styles.link} disabled={setEstimate.isPending}>
          Save
        </button>
        <button type="button" className={styles.link} onClick={() => setEditing(false)}>
          Cancel
        </button>
      </span>
      {invalid && (
        <span id={hintId} role="alert" className={styles.error}>
          {HINT}
        </span>
      )}
      {setEstimate.isError && (
        <span role="alert" className={styles.error}>
          Could not save the estimate.
        </span>
      )}
    </form>
  )
}
