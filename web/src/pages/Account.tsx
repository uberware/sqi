// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import PageHeader from '@/components/PageHeader'
import ErrorBanner from '@/components/ErrorBanner'
import { useToast } from '@/components/Toast'
import { useAuth } from '@/auth/context'
import { ApiError } from '@/api/client'
import { useChangePassword, useUpdateMe } from '@/api/mutations'
import styles from './Account.module.css'

/** Extracts the RFC-7807 `detail` from an apiFetch rejection, if present. */
function problemDetail(err: unknown): string | null {
  if (err instanceof ApiError) return err.detail
  return err instanceof Error ? err.message : null
}

/**
 * Self-service account page: the one place any authenticated role can change
 * its own display name and password. Both forms post to self-scoped routes
 * that resolve the target from the session, so there is no id to get wrong.
 */
export default function Account() {
  const { principal } = useAuth()
  const { showToast } = useToast()
  const changePassword = useChangePassword()
  const updateMe = useUpdateMe()

  const [displayName, setDisplayName] = useState(principal?.display_name ?? '')
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  // A wrong current password is a correctable input mistake, so it renders
  // inline beneath the field rather than firing a toast that can be missed.
  const [passwordError, setPasswordError] = useState<string | null>(null)

  const profileBusy = updateMe.isPending
  const passwordBusy = changePassword.isPending
  const canSavePassword = currentPassword !== '' && newPassword !== '' && !passwordBusy

  const handleProfile = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      try {
        await updateMe.mutateAsync({ display_name: displayName })
        showToast('Profile updated')
      } catch {
        /* surfaced by the ErrorBanner above the form */
      }
    },
    [displayName, updateMe, showToast],
  )

  const handlePassword = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      if (!canSavePassword) return
      setPasswordError(null)
      try {
        await changePassword.mutateAsync({
          current_password: currentPassword,
          new_password: newPassword,
        })
        // Only cleared on success — a failed attempt keeps what was typed so
        // the user can correct one field instead of retyping both.
        setCurrentPassword('')
        setNewPassword('')
        showToast('Password changed. Other devices have been signed out.')
      } catch (err) {
        setPasswordError(problemDetail(err) ?? 'Could not change password')
      }
    },
    [canSavePassword, currentPassword, newPassword, changePassword, showToast],
  )

  return (
    <>
      <PageHeader title="Account" subtitle={principal?.username ?? ''} />

      <form className={styles.form} onSubmit={handleProfile}>
        <h2 className={styles.heading}>Profile</h2>

        {updateMe.isError && (
          <ErrorBanner>
            Failed to update profile: {problemDetail(updateMe.error) ?? 'Unknown error'}
          </ErrorBanner>
        )}

        <div className={styles.field}>
          <label className={styles.label} htmlFor="account-display-name">
            Display name
          </label>
          <input
            id="account-display-name"
            type="text"
            className={styles.input}
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
          />
        </div>

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={profileBusy}>
            {profileBusy ? 'Saving…' : 'Save profile'}
          </button>
        </div>
      </form>

      <form className={styles.form} onSubmit={handlePassword}>
        <h2 className={styles.heading}>Password</h2>
        <p className={styles.hint}>
          Changing your password signs out every other device. API keys are not affected.
        </p>

        <div className={styles.field}>
          <label className={styles.label} htmlFor="account-current-password">
            Current password
          </label>
          <input
            id="account-current-password"
            type="password"
            autoComplete="current-password"
            className={styles.input}
            value={currentPassword}
            onChange={(e) => setCurrentPassword(e.target.value)}
          />
          {passwordError !== null && (
            <p role="alert" className={styles.fieldError}>
              {passwordError}
            </p>
          )}
        </div>

        <div className={styles.field}>
          <label className={styles.label} htmlFor="account-new-password">
            New password
          </label>
          <input
            id="account-new-password"
            type="password"
            autoComplete="new-password"
            className={styles.input}
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
          />
        </div>

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={!canSavePassword}>
            {passwordBusy ? 'Changing…' : 'Change password'}
          </button>
        </div>
      </form>
    </>
  )
}
