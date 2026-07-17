import styles from './App.module.css'
import ErrorBoundary from '@/components/ErrorBoundary'
import Sidebar from '@/components/layout/Sidebar'
import { ToastProvider } from '@/components/Toast'
import AppRoutes from '@/routes'
import { useAuth } from '@/auth/context'
import { WebSocketProvider } from '@/ws/context'
import Login from '@/pages/Login'

export default function App() {
  const { status } = useAuth()

  // `status` is the one signal gating the whole shell. It is 'authed' both
  // for a real user session AND for the anonymous principal auth-off returns
  // from GET /auth/me (kind: 'anonymous') — so the shell renders exactly as
  // it always has when auth is disabled, with no separate feature flag.
  if (status === 'loading') {
    return <div className={styles.loading}>Loading…</div>
  }

  if (status === 'anon') {
    return <Login />
  }

  // The WebSocket connection is only meaningful once there's a session for
  // it to push live updates against, so it's scoped to the authed shell
  // rather than connecting unconditionally from main.tsx before login.
  return (
    <WebSocketProvider>
      <ToastProvider>
        <div className={styles.layout}>
          <Sidebar />
          <main className={styles.main}>
            <ErrorBoundary>
              <AppRoutes />
            </ErrorBoundary>
          </main>
        </div>
      </ToastProvider>
    </WebSocketProvider>
  )
}
