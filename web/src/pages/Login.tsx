// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, type FormEvent } from 'react'
import { useLogin } from '@/api/mutations'
import { useAuthProviders } from '@/api/queries'
import { useAuth } from '@/auth/context'
import { ApiError } from '@/api/client'
import styles from './Login.module.css'

/**
 * The message shown after a failed SSO round trip.
 *
 * Generic on purpose, and it must stay that way. The callback deliberately
 * redirects here with a bare `sso_error=1` and no reason: telling the browser
 * whether the failure was "no such user" or "no role mapping" would turn a
 * public, unauthenticated endpoint into an account-enumeration oracle. The
 * detail goes to the server log, where an operator can read it and a stranger
 * cannot.
 */
const SSO_ERROR_MESSAGE = 'Sign-in failed. Please try again or contact your administrator.'

/**
 * Reads the SSO failure marker the callback redirects with.
 *
 * Deliberately `window.location.search` and not a router hook: this component
 * is mounted inside a router whose location, under MemoryRouter (and in
 * principle any non-browser history), is not the real address bar the callback
 * navigated to. The marker arrives via a full-page redirect from the server, so
 * the document's own URL is the only place it reliably exists.
 *
 * Read once, via a `useState` lazy initializer, so it reflects only the
 * marker present on the component's first render and is not re-evaluated on
 * every subsequent one. That is not the same guarantee module scope would
 * give: it does not survive unmount/remount (a session expiring later and
 * `Login` mounting again re-reads the URL from scratch), which is why the
 * call site also clears the marker once it has been read.
 *
 * Clearing it here, rather than leaving it in the address bar, matters
 * because this is an SPA: without a page load to reset it, the marker would
 * otherwise persist across a subsequent successful login for the rest of the
 * session, and reappear as a false "sign-in failed" if `Login` ever
 * remounts (e.g. the session later expires).
 */
function hasSSOError(): boolean {
  const present = new URLSearchParams(window.location.search).get('sso_error') !== null
  if (present) {
    window.history.replaceState({}, '', window.location.pathname)
  }
  return present
}

/**
 * Rendered by {@link App} in place of the app shell whenever `useAuth()`
 * resolves to `'anon'`. Submits `POST /auth/login`; on success the server
 * sets the HttpOnly session cookie and this component asks the AuthProvider
 * to re-resolve, which flips the app back to the shell. Nothing about the
 * session is stored here — no token, no localStorage.
 *
 * It also renders whatever SSO options `GET /auth/providers` advertises. That
 * query is unauthenticated by necessity — it is what tells this page the SSO
 * button exists, and the user has no credential yet.
 */
export default function Login() {
  const login = useLogin()
  const providers = useAuthProviders()
  const { refresh } = useAuth()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [ssoError] = useState(hasSSOError)

  // Derived from the mutation rather than mirrored into local state:
  // mutateAsync already clears the previous error when a new attempt starts.
  // A password-login error wins over the SSO marker: it describes the attempt
  // the user just made, while the marker describes the one that brought them
  // back to this page.
  let error = ssoError ? SSO_ERROR_MESSAGE : ''
  if (login.isError) {
    error = login.error instanceof ApiError ? login.error.detail : 'Login failed'
  }

  const ssoOptions = providers.data?.sso ?? []

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    try {
      await login.mutateAsync({ username, password })
      refresh()
    } catch {
      /* rendered from login.error above */
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
        {ssoOptions.length > 0 && (
          <>
            <div className={styles.divider} aria-hidden="true">
              or
            </div>
            {ssoOptions.map((p) => (
              // An anchor, not a button with an onClick: the browser must
              // actually navigate to the provider. A fetch would follow the
              // redirect in the background and land the HTML of a login page
              // in a response body.
              <a key={p.id} className={styles.ssoBtn} href={p.login_url}>
                {p.label}
              </a>
            ))}
          </>
        )}
      </div>
    </div>
  )
}
