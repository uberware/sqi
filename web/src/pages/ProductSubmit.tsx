// SPDX-License-Identifier: AGPL-3.0-or-later
import { useEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import ProductParamField from '@/components/ProductParamField'
import { useToast } from '@/components/Toast'
import { useProduct, useProductParameters, useFarmsWithQueues } from '@/api/queries'
import { useSubmitProductJob } from '@/api/mutations'
import { ApiError } from '@/api/client'
import { initialValue, defaultJobName, paramGroup, selectWidget } from '@/lib/productForm'
import { validateAll } from '@/lib/productValidation'
import type { ProductParameter } from '@/api/types'
import styles from './ProductSubmit.module.css'

const QUEUE_STORAGE_KEY = 'sqi:submit:last-queue-id'

export default function ProductSubmit() {
  const { name = '' } = useParams()
  const navigate = useNavigate()
  const { showToast } = useToast()
  const product = useProduct(name)
  const params = useProductParameters(name)
  const farms = useFarmsWithQueues()
  const submit = useSubmitProductJob()

  const [values, setValues] = useState<Record<string, string>>({})
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [jobName, setJobName] = useState('')
  const [queueId, setQueueId] = useState(() => localStorage.getItem(QUEUE_STORAGE_KEY) ?? '')
  const [formError, setFormError] = useState('')

  // Seed parameter defaults once data arrives (useEffect — cleaner than useMemo
  // for side-effects; seeded-ref prevents re-running on subsequent renders).
  const valuesSeeded = useRef(false)
  useEffect(() => {
    const paramList = params.data
    if (!valuesSeeded.current && paramList) {
      valuesSeeded.current = true
      const defaults: Record<string, string> = {}
      for (const p of paramList) defaults[p.name] = initialValue(p)
      setValues(defaults)
    }
  }, [params.data])

  const jobNameSeeded = useRef(false)
  useEffect(() => {
    const productData = product.data
    if (!jobNameSeeded.current && productData) {
      jobNameSeeded.current = true
      setJobName(defaultJobName(productData.title || productData.name))
    }
  }, [product.data])

  if (product.isLoading || params.isLoading) return <p>Loading…</p>

  const productData = product.data
  const paramList = params.data

  if (product.error || params.error || !productData || !paramList) {
    return <p role="alert">Failed to load this product.</p>
  }

  // Fall back to the first available queue when none is stored (mirrors Submit.tsx).
  const farmList = farms.data ?? []
  const allQueues = farmList.flatMap((f) => f.queues)
  const effectiveQueueId = allQueues.some((q) => q.id === queueId)
    ? queueId
    : (allQueues[0]?.id ?? '')
  const farmForQueue = farmList.find((f) => f.queues.some((q) => q.id === effectiveQueueId))

  // Group parameters by userInterface group_label, preserving declaration order.
  const groups: { heading: string; items: ProductParameter[] }[] = []
  for (const p of paramList) {
    const heading = paramGroup(p)
    const existing = groups.find((x) => x.heading === heading)
    if (existing) {
      existing.items.push(p)
    } else {
      groups.push({ heading, items: [p] })
    }
  }

  function setValue(pname: string, v: string) {
    setValues((prev) => ({ ...prev, [pname]: v }))
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setFormError('')
    // TypeScript guard — paramList is narrowed at render time but closures re-widen it.
    if (!paramList) return
    const errs = validateAll(paramList, values)
    setErrors(errs)
    if (Object.keys(errs).length > 0) return
    // Guard: no queues configured (effectiveQueueId = ''); safe early return.
    if (!farmForQueue) return

    try {
      const job = await submit.mutateAsync({
        productName: name,
        name: jobName,
        farmId: farmForQueue.farm.id,
        queueId: effectiveQueueId,
        parameters: Object.fromEntries(
          paramList
            .filter((p) => selectWidget(p) !== 'hidden')
            .map((p) => [p.name, values[p.name] ?? '']),
        ),
      })
      localStorage.setItem(QUEUE_STORAGE_KEY, effectiveQueueId)
      showToast('Job submitted', 'success')
      navigate(`/jobs/${job.id}`)
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : 'Submission failed')
    }
  }

  return (
    <form className={styles.page} onSubmit={(e) => void handleSubmit(e)}>
      <PageHeader title={`Submit: ${productData.title || productData.name}`} />

      <div className={styles.content}>
        <div className={styles.row}>
          <label htmlFor="jobName">Job name</label>
          <input id="jobName" value={jobName} onChange={(e) => setJobName(e.target.value)} />
        </div>

        <div className={styles.row}>
          <label htmlFor="queue">Queue</label>
          <select id="queue" value={effectiveQueueId} onChange={(e) => setQueueId(e.target.value)}>
            {farms.isLoading && <option value="">Loading queues…</option>}
            {!farms.isLoading && allQueues.length === 0 && (
              <option value="">No queues available</option>
            )}
            {farmList.map((f) => (
              <optgroup key={f.farm.id} label={f.farm.name}>
                {f.queues.map((q) => (
                  <option key={q.id} value={q.id}>
                    {q.name}
                  </option>
                ))}
              </optgroup>
            ))}
          </select>
        </div>

        {groups
          .filter((g) => g.items.some((p) => selectWidget(p) !== 'hidden'))
          .map((g) => (
            <section className={styles.group} key={g.heading || '_default'}>
              {g.heading && <h2 className={styles.groupHeading}>{g.heading}</h2>}
              {g.items.map((p) => {
                const fieldError = errors[p.name]
                return (
                  <ProductParamField
                    key={p.name}
                    param={p}
                    value={values[p.name] ?? ''}
                    onChange={(v) => setValue(p.name, v)}
                    {...(fieldError !== undefined ? { error: fieldError } : {})}
                  />
                )
              })}
            </section>
          ))}

        {formError && (
          <p className={styles.formError} role="alert">
            {formError}
          </p>
        )}
        <button type="submit" className={styles.submitBtn} disabled={submit.isPending}>
          Submit job
        </button>
      </div>
    </form>
  )
}
