// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Farm-scoped "Depends on jobs" multi-select, shared by the raw job Submit
 * form and the product Submit form. Candidates come from the farm-scoped job
 * list (excluding terminal jobs — nothing meaningful to wait on there), and
 * the current selection is cleared whenever the target farm changes, since a
 * dependency in a farm the job is no longer targeting would be rejected by
 * the server (422) if it silently rode along.
 *
 * Styling comes from the host page via the className props, matching
 * RetryPolicyFields' pattern, so each form keeps its existing look.
 */

import { useCallback, useEffect, useRef } from 'react'
import { useListJobs } from '@/api/queries'

/** Job statuses that can no longer be depended on meaningfully (already finished). */
const TERMINAL_JOB_STATUSES: ReadonlySet<string> = new Set(['completed', 'failed', 'canceled'])

/** Candidate upstream jobs are capped so the picker never silently truncates. */
const CANDIDATE_LIMIT = 200

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
  selectClassName?: string | undefined
  /** Class for the "no eligible upstream jobs" hint text. */
  noteClassName?: string | undefined
}

export default function DependsOnField({
  farmId,
  value,
  onChange,
  fieldClassName,
  labelClassName,
  selectClassName,
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

  const handleChange = useCallback(
    (e: React.ChangeEvent<HTMLSelectElement>) => {
      onChange(Array.from(e.target.selectedOptions, (option) => option.value))
    },
    [onChange],
  )

  // Switching to a queue in a different farm invalidates any selected
  // upstream jobs (dependencies must be in the same farm) — clear them so a
  // stale, now-hidden selection can't silently ride along on submit.
  const prevFarmIdRef = useRef(farmId)
  useEffect(() => {
    if (prevFarmIdRef.current !== farmId) {
      prevFarmIdRef.current = farmId
      onChange([])
    }
  }, [farmId, onChange])

  return (
    <div className={fieldClassName}>
      <label htmlFor="dependsOn" className={labelClassName}>
        Depends on jobs
      </label>
      <select
        id="dependsOn"
        multiple
        className={selectClassName}
        value={value}
        onChange={handleChange}
        size={4}
        disabled={!farmId || candidates.length === 0}
      >
        {candidates.map((job) => (
          <option key={job.id} value={job.id}>
            {job.name} ({job.status})
          </option>
        ))}
      </select>
      {farmId && candidates.length === 0 && (
        <p className={noteClassName}>No eligible upstream jobs in this farm.</p>
      )}
    </div>
  )
}
