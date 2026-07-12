// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * The three retry-policy override inputs — max attempts, retry delay, and
 * failure limit — shared by the farm, queue, and job-submission forms.
 * Styling comes from the host page via the className props so each form
 * keeps its existing look; label wording and placeholder text likewise
 * stay per-page (`labelVariant`, `placeholder`).
 */

const LABELS = {
  long: {
    maxAttempts: 'Max attempts per task',
    retryDelaySeconds: 'Retry delay (seconds)',
    failureLimit: 'Failure limit (auto-park after N failures)',
  },
  short: {
    maxAttempts: 'Max attempts',
    retryDelaySeconds: 'Retry delay (seconds)',
    failureLimit: 'Failure limit',
  },
} as const

export interface RetryPolicyFieldsProps {
  maxAttempts: string
  onMaxAttemptsChange: (value: string) => void
  retryDelaySeconds: string
  onRetryDelaySecondsChange: (value: string) => void
  failureLimit: string
  onFailureLimitChange: (value: string) => void
  /** Placeholder shown on all three inputs, e.g. "Inherit farm default". */
  placeholder: string
  /** 'long' (default) spells labels out in full; 'short' is the compact wording. */
  labelVariant?: 'long' | 'short' | undefined
  /** Class for each field's wrapping div. */
  fieldClassName?: string | undefined
  labelClassName?: string | undefined
  inputClassName?: string | undefined
}

export default function RetryPolicyFields({
  maxAttempts,
  onMaxAttemptsChange,
  retryDelaySeconds,
  onRetryDelaySecondsChange,
  failureLimit,
  onFailureLimitChange,
  placeholder,
  labelVariant = 'long',
  fieldClassName,
  labelClassName,
  inputClassName,
}: RetryPolicyFieldsProps) {
  const labels = LABELS[labelVariant]
  const fields = [
    {
      id: 'maxAttempts',
      label: labels.maxAttempts,
      min: 1,
      value: maxAttempts,
      onChange: onMaxAttemptsChange,
    },
    {
      id: 'retryDelaySeconds',
      label: labels.retryDelaySeconds,
      min: 0,
      value: retryDelaySeconds,
      onChange: onRetryDelaySecondsChange,
    },
    {
      id: 'failureLimit',
      label: labels.failureLimit,
      // 0 is a meaningful override: "disable the inherited failure limit".
      min: 0,
      value: failureLimit,
      onChange: onFailureLimitChange,
    },
  ]

  return (
    <>
      {fields.map((field) => (
        <div key={field.id} className={fieldClassName}>
          <label htmlFor={field.id} className={labelClassName}>
            {field.label}
          </label>
          <input
            id={field.id}
            type="number"
            min={field.min}
            className={inputClassName}
            value={field.value}
            onChange={(e) => field.onChange(e.target.value)}
            placeholder={placeholder}
          />
        </div>
      ))}
    </>
  )
}
