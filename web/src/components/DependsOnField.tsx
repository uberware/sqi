// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Farm-scoped "Depends on jobs" checkbox list, shared by the raw job Submit
 * form and the product Submit form. Candidates come from the farm-scoped job
 * list (excluding terminal jobs — nothing meaningful to wait on there), and
 * the current selection is cleared whenever the target farm changes, since a
 * dependency in a farm the job is no longer targeting would be rejected by
 * the server (422) if it silently rode along.
 *
 * Rendered as a scrollable checklist (rather than a native <select multiple>)
 * so every candidate and its checked state is visible at a glance. The list
 * owns its height via a co-located CSS module; the host page only supplies the
 * field wrapper / label / note classes to match each form's surrounding look.
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { useListJobs } from '@/api/queries'
import { filterBySearch } from '@/utils/filterBySearch'
import styles from './DependsOnField.module.css'

/** Job statuses that can no longer be depended on meaningfully (already finished). */
const TERMINAL_JOB_STATUSES: ReadonlySet<string> = new Set(['completed', 'failed', 'canceled'])

/** Candidate upstream jobs are capped so the picker never silently truncates. */
const CANDIDATE_LIMIT = 200

const LABEL_ID = 'dependsOn-label'

export interface DependsOnFieldProps {
  /** Farm the target queue belongs to; candidates are scoped to this farm. Undefined disables the field. */
  farmId: string | undefined
  /** Currently selected upstream job IDs. */
  value: string[]
  /** Called with the new selection, and with `[]` when a farm change invalidates the current one. */
  onChange: (ids: string[]) => void
  /** Class for the field's wrapping element. */
  fieldClassName?: string | undefined
  labelClassName?: string | undefined
  /** Class for the "no eligible upstream jobs" hint text. */
  noteClassName?: string | undefined
}

/** Joins optional class names, dropping the undefined ones. */
function cx(...classes: (string | undefined)[]): string {
  return classes.filter(Boolean).join(' ')
}

export default function DependsOnField({
  farmId,
  value,
  onChange,
  fieldClassName,
  labelClassName,
  noteClassName,
}: DependsOnFieldProps) {
  const { data: candidateJobsPage } = useListJobs(
    {
      ...(farmId ? { farm_id: farmId } : {}),
      limit: CANDIDATE_LIMIT,
    },
    { enabled: Boolean(farmId) },
  )
  const candidates = (candidateJobsPage?.items ?? []).filter(
    (job) => !TERMINAL_JOB_STATUSES.has(job.status),
  )

  // Free-text filter: each whitespace-separated word is an independent,
  // case-insensitive substring term, ANDed and matched anywhere in the job's
  // name/status (shared multi-term matcher). So "foo bar" keeps
  // "fool-grabbing-rebar" and "barfoo".
  const [filter, setFilter] = useState('')
  const shown = filterBySearch(candidates, filter, (job) => [job.name, job.status])

  const toggle = useCallback(
    (id: string) => {
      onChange(value.includes(id) ? value.filter((v) => v !== id) : [...value, id])
    },
    [onChange, value],
  )

  // Switching to a queue in a different farm invalidates any selected
  // upstream jobs (dependencies must be in the same farm) — clear them so a
  // stale, now-hidden selection can't silently ride along on submit.
  const prevFarmIdRef = useRef(farmId)
  useEffect(() => {
    if (prevFarmIdRef.current !== farmId) {
      prevFarmIdRef.current = farmId
      onChange([])
      setFilter('')
    }
  }, [farmId, onChange])

  const note = (text: string) => (
    <div
      role="group"
      aria-labelledby={LABEL_ID}
      aria-disabled="true"
      className={cx(styles.list, styles.listEmpty)}
    >
      <p className={cx(noteClassName, noteClassName ? undefined : styles.note)}>{text}</p>
    </div>
  )

  return (
    <div className={fieldClassName}>
      <span id={LABEL_ID} className={labelClassName}>
        Depends on jobs
      </span>
      {!farmId ? (
        note('Select a farm and queue first to choose dependencies.')
      ) : candidates.length === 0 ? (
        note('No eligible upstream jobs in this farm.')
      ) : (
        <>
          <input
            type="search"
            className={styles.filter}
            placeholder="Filter jobs…"
            aria-label="Filter jobs"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
          <div role="group" aria-labelledby={LABEL_ID} className={styles.list}>
            {shown.length === 0 ? (
              <p className={cx(noteClassName, noteClassName ? undefined : styles.note)}>
                No jobs match the filter.
              </p>
            ) : (
              shown.map((job) => (
                <label key={job.id} className={styles.row}>
                  <input
                    type="checkbox"
                    className={styles.checkbox}
                    checked={value.includes(job.id)}
                    onChange={() => toggle(job.id)}
                  />
                  <span className={styles.name}>{job.name}</span>
                  <span className={styles.status}>{job.status}</span>
                </label>
              ))
            )}
          </div>
        </>
      )}
    </div>
  )
}
