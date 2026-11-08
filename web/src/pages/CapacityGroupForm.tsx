// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'react'
import type { ReactNode } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import { useGetCapacityGroup, useListAllFarmQueues, useListFarms } from '@/api/queries'
import { useCreateCapacityGroup, useUpdateCapacityGroup } from '@/api/mutations'
import type { CapacityGroup, Farm } from '@/api/types'
import {
  buildInput,
  defaultValues,
  mapServerErrors,
  validateValues,
  valuesFromGroup,
} from '@/lib/capacityGroupForm'
import type { FieldErrors, FormValues } from '@/lib/capacityGroupForm'
import styles from './CapacityGroupForm.module.css'

interface Props {
  mode: 'create' | 'edit'
}

interface A11yProps {
  id: string
  'aria-describedby': string | undefined
  'aria-invalid': true | undefined
}

interface FieldProps {
  id: string
  label: string
  error?: string | undefined
  hint?: string
  children: (a11y: A11yProps) => ReactNode
}

/** A labelled control with an optional hint and error, both announced with it. */
function Field({ id, label, error, hint, children }: FieldProps) {
  const hintId = `${id}-hint`
  const errorId = `${id}-error`
  const describedBy = [hint !== undefined ? hintId : '', error !== undefined ? errorId : '']
    .filter(Boolean)
    .join(' ')
  return (
    <div className={styles.field}>
      <label htmlFor={id} className={styles.label}>
        {label}
      </label>
      {children({
        id,
        'aria-describedby': describedBy === '' ? undefined : describedBy,
        'aria-invalid': error !== undefined ? true : undefined,
      })}
      {hint !== undefined && (
        <p id={hintId} className={styles.hint}>
          {hint}
        </p>
      )}
      {error !== undefined && (
        <p id={errorId} className={styles.error}>
          {error}
        </p>
      )}
    </div>
  )
}

interface InputFieldProps {
  id: string
  label: string
  value: string
  onChange: (v: string) => void
  error?: string | undefined
  hint?: string
  type?: 'text' | 'number'
  min?: number
  required?: boolean
}

function InputField({
  id,
  label,
  value,
  onChange,
  error,
  hint,
  type = 'text',
  min,
  required,
}: InputFieldProps) {
  return (
    <Field id={id} label={label} error={error} {...(hint !== undefined ? { hint } : {})}>
      {(a11y) => (
        <input
          {...a11y}
          type={type}
          className={styles.input}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          {...(min !== undefined ? { min } : {})}
          {...(required ? { required: true, 'aria-required': true } : {})}
        />
      )}
    </Field>
  )
}

interface ProfileFieldsProps {
  values: FormValues
  errors: FieldErrors
  change: (key: keyof FormValues, value: string) => void
}

function ProfileFields({ values, errors, change }: ProfileFieldsProps) {
  return (
    <fieldset className={styles.fieldset}>
      <legend className={styles.legend}>Machine profile</legend>
      <p className={styles.hint}>
        What one machine of this group offers. Members that fall short are reported on the group
        page.
      </p>
      <InputField
        id="cpuCount"
        label="CPU count"
        type="number"
        min={1}
        required
        value={values.cpuCount}
        onChange={(v) => change('cpuCount', v)}
        error={errors.cpuCount}
      />
      <InputField
        id="ramMb"
        label="RAM (MB)"
        type="number"
        min={0}
        value={values.ramMb}
        onChange={(v) => change('ramMb', v)}
        error={errors.ramMb}
      />
      <InputField
        id="gpuCount"
        label="GPU count"
        type="number"
        min={0}
        value={values.gpuCount}
        onChange={(v) => change('gpuCount', v)}
        error={errors.gpuCount}
      />
      <InputField
        id="gpuMemoryMb"
        label="GPU memory (MB)"
        type="number"
        min={0}
        value={values.gpuMemoryMb}
        onChange={(v) => change('gpuMemoryMb', v)}
        error={errors.gpuMemoryMb}
      />
      <InputField
        id="os"
        label="OS"
        value={values.os}
        onChange={(v) => change('os', v)}
        hint="For example linux, windows or macos."
        error={errors.os}
      />
      <InputField
        id="arch"
        label="Arch"
        value={values.arch}
        onChange={(v) => change('arch', v)}
        hint="For example amd64 or arm64."
        error={errors.arch}
      />
      <InputField
        id="computeLocation"
        label="Compute location"
        value={values.computeLocation}
        onChange={(v) => change('computeLocation', v)}
        error={errors.computeLocation}
      />
      <Field id="tags" label="Tags" hint="One key=value per line." error={errors.tags}>
        {(a11y) => (
          <textarea
            {...a11y}
            className={styles.textarea}
            rows={3}
            value={values.tags}
            onChange={(e) => change('tags', e.target.value)}
          />
        )}
      </Field>
      <InputField
        id="extensions"
        label="Extensions"
        value={values.extensions}
        onChange={(v) => change('extensions', v)}
        hint="Comma-separated OpenJD extension names the machines support."
        error={errors.extensions}
      />
    </fieldset>
  )
}

interface QueuePickerProps {
  farmId: string
  selected: string[]
  onChange: (ids: string[]) => void
  error?: string | undefined
}

/** Checkboxes for the farm's queues. Nothing checked means every queue in the farm. */
function QueuePicker({ farmId, selected, onChange, error }: QueuePickerProps) {
  const { data, isLoading } = useListAllFarmQueues(farmId)
  const queues = data?.items ?? []
  // A saved queue the list does not return stays selected, shown by its id, so
  // a save does not drop it.
  const known = new Set(queues.map((q) => q.id))
  const orphans = selected.filter((id) => !known.has(id))
  const entries = [
    ...queues.map((q) => ({ id: q.id, name: q.name })),
    ...(isLoading ? [] : orphans.map((id) => ({ id, name: id }))),
  ]

  function toggle(id: string) {
    onChange(selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id])
  }

  return (
    <fieldset className={styles.fieldset} aria-describedby="queueIds-hint">
      <legend className={styles.legend}>Queues</legend>
      <p id="queueIds-hint" className={styles.hint}>
        The queues whose demand this group serves. Leave all unchecked to serve every queue in the
        farm.
      </p>
      {isLoading && <p className={styles.hint}>Loading queues…</p>}
      {entries.map((q) => (
        <label key={q.id} className={styles.checkRow}>
          <input type="checkbox" checked={selected.includes(q.id)} onChange={() => toggle(q.id)} />
          {q.name}
        </label>
      ))}
      {error !== undefined && <p className={styles.error}>{error}</p>}
    </fieldset>
  )
}

interface InnerProps {
  /** The group being edited; absent on create. */
  original?: CapacityGroup
  farms: Farm[]
  defaults: FormValues
}

function CapacityGroupFormInner({ original, farms, defaults }: InnerProps) {
  const navigate = useNavigate()
  const { showToast } = useToast()
  const createGroup = useCreateCapacityGroup()
  const updateGroup = useUpdateCapacityGroup()

  const [values, setValues] = useState<FormValues>(defaults)
  const [serverFields, setServerFields] = useState<FieldErrors>({})
  const [serverForm, setServerForm] = useState<string[]>([])

  const clientErrors = validateValues(values)
  const isPending = createGroup.isPending || updateGroup.isPending
  const canSubmit = Object.keys(clientErrors).length === 0 && !isPending
  const isEdit = original !== undefined

  function change(key: keyof FormValues, value: string | boolean | string[]) {
    setValues((v) => ({ ...v, [key]: value }))
    if (key === 'enabled') return
    setServerFields((prev) =>
      prev[key] === undefined
        ? prev
        : (Object.fromEntries(Object.entries(prev).filter(([k]) => k !== key)) as FieldErrors),
    )
  }

  function changeFarm(farmId: string) {
    setValues((v) => ({ ...v, farmId, queueIds: [] }))
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    const input = buildInput(values, original)
    setServerFields({})
    setServerForm([])
    try {
      if (original === undefined) {
        await createGroup.mutateAsync(input)
        showToast(`Capacity group "${input.name}" created`, 'success')
      } else {
        await updateGroup.mutateAsync({ id: original.id, input })
        showToast(`Capacity group "${input.name}" saved`, 'success')
      }
      navigate('/capacity-groups')
    } catch (err) {
      const detail = err instanceof Error ? err.message : 'Save failed'
      const mapped = mapServerErrors(detail)
      setServerFields(mapped.fields)
      setServerForm(mapped.form)
    }
  }

  const fieldErrors: FieldErrors = {
    ...(clientErrors.tags ? { tags: clientErrors.tags } : {}),
    ...serverFields,
  }
  const hasServerError = serverForm.length > 0 || Object.keys(serverFields).length > 0
  // If the group's farm is missing from the farm list, keep it as an option so
  // the control still shows which farm the group belongs to.
  const farmOptions = farms.some((f) => f.id === values.farmId)
    ? farms
    : [...farms, ...(values.farmId !== '' ? [{ id: values.farmId, name: values.farmId }] : [])]

  return (
    <div className={styles.page}>
      <PageHeader
        title={isEdit ? 'Edit Capacity Group' : 'New Capacity Group'}
        subtitle="A capacity group is a set of machines sqi follows demand with"
      />

      <form className={styles.form} onSubmit={(e) => void handleSubmit(e)} noValidate>
        {hasServerError && (
          <div className={styles.formError} role="alert">
            <p>The server did not accept these settings.</p>
            {serverForm.map((line) => (
              <p key={line}>{line}</p>
            ))}
          </div>
        )}

        <Field id="farm" label="Farm" error={fieldErrors.farmId}>
          {(a11y) => (
            <select
              {...a11y}
              className={styles.select}
              value={values.farmId}
              onChange={(e) => changeFarm(e.target.value)}
              disabled={isEdit}
              required
              aria-required="true"
            >
              {farmOptions.map((f) => (
                <option key={f.id} value={f.id}>
                  {f.name}
                </option>
              ))}
            </select>
          )}
        </Field>

        <InputField
          id="name"
          label="Name"
          required
          value={values.name}
          onChange={(v) => change('name', v)}
          hint="Letters, digits, '.', '_' or '-', up to 63 characters. Workers use it with worker.capacity_group."
          error={fieldErrors.name}
        />

        <Field
          id="kind"
          label="Kind"
          error={fieldErrors.kind}
          hint={
            isEdit
              ? 'The kind cannot be changed here.'
              : 'Sized: interchangeable machines that join with a group token. Named: specific machines you add by name.'
          }
        >
          {(a11y) => (
            <select
              {...a11y}
              className={styles.select}
              value={values.kind}
              onChange={(e) => change('kind', e.target.value)}
              disabled={isEdit}
            >
              <option value="sized">sized</option>
              <option value="named">named</option>
            </select>
          )}
        </Field>

        <Field id="provider" label="Provider" error={fieldErrors.provider}>
          {(a11y) => (
            <select
              {...a11y}
              className={styles.select}
              value={values.provider}
              onChange={(e) => change('provider', e.target.value)}
            >
              <option value="external">external</option>
            </select>
          )}
        </Field>

        <label className={styles.checkRow}>
          <input
            type="checkbox"
            checked={values.enabled}
            onChange={(e) => change('enabled', e.target.checked)}
          />
          Enabled
        </label>

        <QueuePicker
          farmId={values.farmId}
          selected={values.queueIds}
          onChange={(ids) => change('queueIds', ids)}
          error={fieldErrors.queueIds}
        />

        <InputField
          id="minMachines"
          label="Min machines"
          type="number"
          min={0}
          required
          value={values.minMachines}
          onChange={(v) => change('minMachines', v)}
          hint="Kept up regardless of demand."
          error={fieldErrors.minMachines}
        />
        <InputField
          id="maxMachines"
          label="Max machines"
          type="number"
          min={1}
          required
          value={values.maxMachines}
          onChange={(v) => change('maxMachines', v)}
          error={fieldErrors.maxMachines}
        />
        <InputField
          id="priority"
          label="Priority"
          type="number"
          required
          value={values.priority}
          onChange={(v) => change('priority', v)}
          hint="Higher is preferred when groups compete for the same demand."
          error={fieldErrors.priority}
        />
        <InputField
          id="maxStepUp"
          label="Max step up"
          type="number"
          min={1}
          required
          value={values.maxStepUp}
          onChange={(v) => change('maxStepUp', v)}
          hint="The most machines one scale-up decision may add."
          error={fieldErrors.maxStepUp}
        />
        <InputField
          id="cooldown"
          label="Scale-up cooldown (seconds)"
          type="number"
          min={0}
          required
          value={values.cooldown}
          onChange={(v) => change('cooldown', v)}
          error={fieldErrors.cooldown}
        />
        <InputField
          id="idle"
          label="Idle timeout (seconds)"
          type="number"
          min={60}
          required
          value={values.idle}
          onChange={(v) => change('idle', v)}
          hint="How long a machine sits idle before it is a retire candidate."
          error={fieldErrors.idle}
        />
        <InputField
          id="boot"
          label="Boot timeout (seconds)"
          type="number"
          min={60}
          required
          value={values.boot}
          onChange={(v) => change('boot', v)}
          hint="How long a started machine has to register."
          error={fieldErrors.boot}
        />

        <Field id="retireAction" label="Retire action" error={fieldErrors.retireAction}>
          {(a11y) => (
            <select
              {...a11y}
              className={styles.select}
              value={values.retireAction}
              onChange={(e) => change('retireAction', e.target.value)}
            >
              <option value="sleep">sleep</option>
              <option value="shutdown">shutdown</option>
            </select>
          )}
        </Field>

        {values.kind === 'sized' ? (
          <ProfileFields values={values} errors={fieldErrors} change={change} />
        ) : (
          <p className={styles.hint}>
            A named group&rsquo;s machine profile is learned from the group&rsquo;s members, so
            there is nothing to enter here.
          </p>
        )}

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={!canSubmit}>
            {isEdit ? 'Save' : 'Create Capacity Group'}
          </button>
          <Link
            to={isEdit ? `/capacity-groups/${original.id}` : '/capacity-groups'}
            className={styles.cancelBtn}
          >
            Cancel
          </Link>
        </div>
      </form>
    </div>
  )
}

export default function CapacityGroupForm({ mode }: Props) {
  const params = useParams<{ id: string }>()
  const id = params.id ?? ''
  const group = useGetCapacityGroup(mode === 'edit' ? id : '', { poll: false })
  const farmsQuery = useListFarms()

  if (mode === 'edit') {
    if (group.isError) {
      return (
        <div className={styles.page}>
          <p>Failed to load capacity group.</p>
        </div>
      )
    }
    if (group.isLoading || !group.data || farmsQuery.isLoading) {
      return (
        <div className={styles.page}>
          <p>Loading…</p>
        </div>
      )
    }
    return (
      <CapacityGroupFormInner
        key={id}
        original={group.data}
        farms={farmsQuery.data ?? []}
        defaults={valuesFromGroup(group.data)}
      />
    )
  }

  if (farmsQuery.isLoading) {
    return (
      <div className={styles.page}>
        <p>Loading…</p>
      </div>
    )
  }
  const farms = farmsQuery.data ?? []
  return <CapacityGroupFormInner farms={farms} defaults={defaultValues(farms[0]?.id ?? '')} />
}
