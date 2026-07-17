// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, type FormEvent } from 'react'
import { useLogin } from '@/api/mutations'
import { useAuth } from '@/auth/context'
import { ApiError } from '@/api/client'
import styles from './Login.module.css'

/**
 * Rendered by {@link App} in place of the app shell whenever `useAuth()`
 * resolves to `'anon'`. Submits `POST /auth/login`; on success the server
 * sets the HttpOnly session cookie and this component asks the AuthProvider
 * to re-resolve, which flips the app back to the shell. Nothing about the
 * session is stored here — no token, no localStorage.
 */
export default function Login() {
  const login = useLogin()
  const { refresh } = useAuth()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    setError('')
    try {
      await login.mutateAsync({ username, password })
      refresh()
    } catch (err) {
      setError(err instanceof ApiError ? err.detail : 'Login failed')
    }
  }

  return (
    <div className={styles.page}>
      <div className={styles.card}>
        <h1 className={styles.title}>Sign in to sqi</h1>
        {error !== '' && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}
        <form className={styles.form} onSubmit={(e) => void handleSubmit(e)}>
          <div className={styles.field}>
            <label htmlFor="login-username" className={styles.label}>
              Username
            </label>
            <input
              id="login-username"
              className={styles.input}
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              required
            />
          </div>
          <div className={styles.field}>
            <label htmlFor="login-password" className={styles.label}>
              Password
            </label>
            <input
              id="login-password"
              type="password"
              className={styles.input}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
              required
            />
          </div>
          <button type="submit" className={styles.submitBtn} disabled={login.isPending}>
            {login.isPending ? 'Signing in…' : 'Log in'}
          </button>
        </form>
      </div>
    </div>
  )
}
