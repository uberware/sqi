// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback } from 'react'
import { useNavigate, useParams } from 'react-router'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import { usePreset } from '@/api/queries'
import { useInstallPreset } from '@/api/mutations'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import styles from './PresetDetail.module.css'

export default function PresetDetail() {
  const params = useParams<{ name: string }>()
  const name = params.name ?? ''
  const navigate = useNavigate()
  const { showToast } = useToast()
  const { principal } = useAuth()
  const canManage = can(principal, 'products.manage')
  const { data: preset, isLoading, isError } = usePreset(name)
  const install = useInstallPreset()

  const handleInstall = useCallback(async () => {
    try {
      const product = await install.mutateAsync(name)
      showToast(`Installed "${product.name}"`, 'success')
      navigate(`/products/${encodeURIComponent(product.name)}`)
    } catch (e) {
      showToast(e instanceof Error ? e.message : 'Failed to install preset', 'error')
    }
  }, [install, name, navigate, showToast])

  if (isLoading || !preset) {
    return (
      <div className={styles.page}>{isError ? <p>Failed to load preset.</p> : <p>Loading…</p>}</div>
    )
  }

  const label =
    preset.status === 'update_available'
      ? 'Update'
      : preset.status === 'installed'
        ? 'Reinstall'
        : 'Install'

  return (
    <div className={styles.page}>
      <PageHeader
        title="Preset Details"
        backTo="/presets"
        backLabel="Presets"
        action={
          canManage ? (
            <button
              type="button"
              className={styles.actionBtn}
              onClick={() => void handleInstall()}
              disabled={install.isPending}
            >
              {label}
            </button>
          ) : undefined
        }
      />

      <div className={styles.nameArea}>
        <h2 className={styles.presetTitle}>{preset.title}</h2>
        {preset.description && <p className={styles.presetDescription}>{preset.description}</p>}
      </div>

      <dl className={styles.meta}>
        <dt>Name</dt>
        <dd>{preset.name}</dd>
        <dt>Category</dt>
        <dd>{preset.category || '—'}</dd>
        <dt>Version</dt>
        <dd>{preset.version || '—'}</dd>
        <dt>Status</dt>
        <dd>{preset.status}</dd>
      </dl>

      <h2 className={styles.sectionTitle}>OpenJD Template</h2>
      <pre className={styles.template} aria-label="OpenJD template">
        {preset.template}
      </pre>
    </div>
  )
}
