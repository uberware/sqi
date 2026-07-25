// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { useNavigate } from 'react-router'
import PageHeader from '@/components/PageHeader'
import CodeEditor from '@/components/CodeEditor'
import RetryPolicyFields from '@/components/RetryPolicyFields'
import DependsOnField from '@/components/DependsOnField'
import { useToast } from '@/components/Toast'
import { useFarmsWithQueues } from '@/api/queries'
import { useSubmitJob } from '@/api/mutations'
import { ApiError } from '@/api/client'
import { detectFormat } from '@/lib/format'
import { parseOptionalInt } from '@/lib/parse'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import type { TemplateFormat } from '@/api/types'
import styles from './Submit.module.css'

// ── Constants ─────────────────────────────────────────────────────────────────

const QUEUE_STORAGE_KEY = 'sqi:submit:last-queue-id'

const EXAMPLES: Record<'shell' | 'parameter-space' | 'usage-limit', string> = {
  shell: `specificationVersion: "jobtemplate-2023-09"
name: hello-world
steps:
  - name: main
    script:
      actions:
        onRun:
          command: echo
          args:
            - "Hello, world!"
`,
  'parameter-space': `specificationVersion: "jobtemplate-2023-09"
name: frame-render
steps:
  - name: render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-10"
    script:
      actions:
        onRun:
          command: echo
          args:
            - "Rendering frame {{Param.Frame}}"
`,
  'usage-limit': `specificationVersion: "jobtemplate-2023-09"
name: licensed-render
steps:
  - name: render
    # Require a free slot in a usage pool before each task is dispatched.
    # The text after "amount.worker.usagepool." is the pool NAME and must
    # match a pool registered on the Usage Pools page (here: "arnold").
    hostRequirements:
      amounts:
        - name: amount.worker.usagepool.arnold   # <-- "arnold" = pool name
          min: 1
    script:
      actions:
        onRun:
          command: echo
          args:
            - "Rendering while holding an Arnold license"
`,
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function readStoredQueueId(): string {
  return localStorage.getItem(QUEUE_STORAGE_KEY) ?? ''
}

function persistQueueId(id: string): void {
  localStorage.setItem(QUEUE_STORAGE_KEY, id)
}

// ── Component ─────────────────────────────────────────────────────────────────

export default function Submit() {
  const { principal } = useAuth()
  const maySubmitAs = can(principal, 'jobs.submit_as')

  const {
    data: farmsWithQueues,
    isLoading: queuesLoading,
    isError: queuesError,
  } = useFarmsWithQueues()

  // Persisted queue preference. Falls back to the first available queue when
  // the stored value is absent or no longer in the list (e.g. queue deleted).
  const [storedQueueId, setStoredQueueId] = useState<string>(readStoredQueueId)

  const allQueues = farmsWithQueues?.flatMap(({ queues }) => queues) ?? []

  // Derive the selected queue without a setState-in-effect: if the stored ID
  // is still in the list, use it; otherwise fall back to the first queue.
  const effectiveQueueId = allQueues.some((q) => q.id === storedQueueId)
    ? storedQueueId
    : (allQueues[0]?.id ?? '')

  const selectedQueue = allQueues.find((q) => q.id === effectiveQueueId)

  // Metadata fields
  const [template, setTemplate] = useState('')
  const [owner, setOwner] = useState('')
  const [priority, setPriority] = useState('')
  const [project, setProject] = useState('')
  const [maxAttempts, setMaxAttempts] = useState('')
  const [retryDelaySeconds, setRetryDelaySeconds] = useState('')
  const [failureLimit, setFailureLimit] = useState('')
  const [dependsOn, setDependsOn] = useState<string[]>([])

  // Example loader select — controlled so it resets to placeholder after each pick.
  const [exampleKey, setExampleKey] = useState('')

  const handleExampleChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    const key = e.target.value
    if (key === 'shell' || key === 'parameter-space' || key === 'usage-limit') {
      setTemplate(EXAMPLES[key])
    }
    setExampleKey('')
  }, [])

  const handleQueueChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    const id = e.target.value
    setStoredQueueId(id)
    persistQueueId(id)
  }, [])

  const submitJob = useSubmitJob()
  const navigate = useNavigate()
  const { showToast } = useToast()

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      if (!selectedQueue) return

      const trimmedTemplate = template.trim()
      if (!trimmedTemplate) return

      const format: TemplateFormat = detectFormat(template)

      try {
        const trimmedOwner = owner.trim()
        const trimmedProject = project.trim()
        const parsedPriority = parseOptionalInt(priority)
        const parsedMaxAttempts = parseOptionalInt(maxAttempts)
        const parsedRetryDelaySeconds = parseOptionalInt(retryDelaySeconds)
        const parsedFailureLimit = parseOptionalInt(failureLimit)

        const job = await submitJob.mutateAsync({
          farmId: selectedQueue.farm_id,
          queueId: selectedQueue.id,
          template,
          format,
          maySubmitAs,
          ...(maySubmitAs && trimmedOwner ? { owner: trimmedOwner } : {}),
          ...(parsedPriority !== undefined ? { priority: parsedPriority } : {}),
          ...(trimmedProject ? { project: trimmedProject } : {}),
          ...(parsedMaxAttempts !== undefined ? { maxAttempts: parsedMaxAttempts } : {}),
          ...(parsedRetryDelaySeconds !== undefined
            ? { retryDelaySeconds: parsedRetryDelaySeconds }
            : {}),
          ...(parsedFailureLimit !== undefined ? { failureLimit: parsedFailureLimit } : {}),
          ...(dependsOn.length > 0 ? { dependsOn } : {}),
        })
        showToast(`Job submitted — ID: ${job.id}`, 'success')
        navigate(`/jobs/${job.id}`)
      } catch {
        // Error is surfaced via submitJob.isError / submitJob.error below.
      }
    },
    [
      selectedQueue,
      template,
      owner,
      maySubmitAs,
      priority,
      project,
      maxAttempts,
      retryDelaySeconds,
      failureLimit,
      dependsOn,
      submitJob,
      navigate,
      showToast,
    ],
  )

  const canSubmit = Boolean(selectedQueue) && template.trim().length > 0 && !submitJob.isPending

  return (
    <div className={styles.page}>
      <PageHeader title="Submit Job" subtitle="Submit a raw OpenJD job template" />

      <form className={styles.form} onSubmit={(e) => void handleSubmit(e)} noValidate>
        <div className={styles.columns}>
          {/* ── Left panel: queue + metadata ── */}
          <div className={styles.leftPanel}>
            <section className={styles.section}>
              <h2 className={styles.sectionTitle}>Target Queue</h2>

              {queuesError && (
                <p className={styles.fieldError} role="alert">
                  Failed to load queues. Check server connectivity.
                </p>
              )}

              <div className={styles.field}>
                <label htmlFor="queue" className={styles.label}>
                  Queue
                </label>
                <select
                  id="queue"
                  className={styles.select}
                  value={effectiveQueueId}
                  onChange={handleQueueChange}
                  disabled={queuesLoading || queuesError}
                  required
                  aria-required="true"
                >
                  {queuesLoading && <option value="">Loading queues…</option>}
                  {!queuesLoading && !farmsWithQueues?.length && (
                    <option value="">No queues available</option>
                  )}
                  {farmsWithQueues?.map(({ farm, queues }) => (
                    <optgroup key={farm.id} label={farm.name}>
                      {queues.map((queue) => (
                        <option key={queue.id} value={queue.id}>
                          {queue.name}
                          {queue.paused ? ' (paused)' : ''}
                        </option>
                      ))}
                    </optgroup>
                  ))}
                </select>
              </div>
            </section>

            <section className={styles.section}>
              <h2 className={styles.sectionTitle}>Job Metadata</h2>
              <p className={styles.sectionNote}>All fields are optional.</p>

              {maySubmitAs && (
                <div className={styles.field}>
                  <label htmlFor="owner" className={styles.label}>
                    Owner
                  </label>
                  <input
                    id="owner"
                    type="text"
                    className={styles.input}
                    value={owner}
                    onChange={(e) => setOwner(e.target.value)}
                    placeholder="username"
                    autoComplete="username"
                  />
                </div>
              )}

              <div className={styles.field}>
                <label htmlFor="priority" className={styles.label}>
                  Priority
                </label>
                <input
                  id="priority"
                  type="number"
                  className={styles.input}
                  value={priority}
                  onChange={(e) => setPriority(e.target.value)}
                  placeholder="50"
                  min={0}
                  max={100}
                />
              </div>

              <div className={styles.field}>
                <label htmlFor="project" className={styles.label}>
                  Project tag
                </label>
                <input
                  id="project"
                  type="text"
                  className={styles.input}
                  value={project}
                  onChange={(e) => setProject(e.target.value)}
                  placeholder="my-project"
                />
              </div>
            </section>

            <section className={styles.section}>
              <h2 className={styles.sectionTitle}>Retry Policy</h2>
              <p className={styles.sectionNote}>
                Optional overrides; unset fields inherit from the queue/farm/server.
              </p>

              <RetryPolicyFields
                maxAttempts={maxAttempts}
                onMaxAttemptsChange={setMaxAttempts}
                retryDelaySeconds={retryDelaySeconds}
                onRetryDelaySecondsChange={setRetryDelaySeconds}
                failureLimit={failureLimit}
                onFailureLimitChange={setFailureLimit}
                placeholder="Inherit"
                fieldClassName={styles.field}
                labelClassName={styles.label}
                inputClassName={styles.input}
              />
            </section>

            <section className={styles.section}>
              <h2 className={styles.sectionTitle}>Dependencies</h2>
              <p className={styles.sectionNote}>
                Optional — this job waits until every selected upstream job finishes.
              </p>

              <DependsOnField
                farmId={selectedQueue?.farm_id}
                value={dependsOn}
                onChange={setDependsOn}
                fieldClassName={styles.field}
                labelClassName={styles.label}
                noteClassName={styles.sectionNote}
              />
            </section>
          </div>

          {/* ── Right panel: OpenJD editor ── */}
          <div className={styles.rightPanel}>
            <div className={styles.editorHeader}>
              <span className={styles.editorTitle}>OpenJD Template</span>
              <select
                className={styles.exampleSelect}
                value={exampleKey}
                onChange={handleExampleChange}
                aria-label="Load example template"
              >
                <option value="">Load example…</option>
                <option value="shell">Single-step shell command</option>
                <option value="parameter-space">Multi-task parameter space</option>
                <option value="usage-limit">Step with a usage limit</option>
              </select>
            </div>

            <div className={styles.editorWrap}>
              <CodeEditor
                value={template}
                onChange={setTemplate}
                aria-label="OpenJD template"
                data-testid="template-editor"
              />
            </div>

            {submitJob.isError && (
              <div className={styles.errorBlock} role="alert">
                <strong className={styles.errorTitle}>Submission failed</strong>
                <pre className={styles.errorDetail}>
                  {submitJob.error instanceof ApiError
                    ? submitJob.error.detail
                    : String(submitJob.error)}
                </pre>
              </div>
            )}
          </div>
        </div>

        <div className={styles.formFooter}>
          <button
            type="submit"
            className={styles.submitBtn}
            disabled={!canSubmit}
            aria-disabled={!canSubmit}
          >
            {submitJob.isPending ? 'Submitting…' : 'Submit Job'}
          </button>
        </div>
      </form>
    </div>
  )
}
