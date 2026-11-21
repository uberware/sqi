// SPDX-License-Identifier: AGPL-3.0-or-later

import { useId } from 'react'
import styles from './SubmitPausedField.module.css'

export interface SubmitPausedFieldProps {
  checked: boolean
  onChange: (checked: boolean) => void
  /** Class for the wrapping div, so the field matches its host form. */
  fieldClassName?: string | undefined
  /** Class for the explanatory note under the checkbox, the host form's note style. */
  noteClassName?: string | undefined
}

/**
 * The "Submit paused" checkbox shared by the raw-template and product submit
 * forms. A job submitted paused is created paused on the server, so nothing
 * is leased until someone resumes it.
 */
export default function SubmitPausedField({
  checked,
  onChange,
  fieldClassName,
  noteClassName,
}: SubmitPausedFieldProps) {
  const noteId = useId()
  return (
    <div className={fieldClassName}>
      <label className={styles.label}>
        <input
          type="checkbox"
          checked={checked}
          onChange={(e) => onChange(e.target.checked)}
          aria-describedby={noteId}
        />
        Submit paused
      </label>
      <p id={noteId} className={noteClassName}>
        Nothing runs until you resume the job.
      </p>
    </div>
  )
}
