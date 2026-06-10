// SPDX-License-Identifier: AGPL-3.0-only
/* eslint-disable react-refresh/only-export-components -- context files export both provider and hooks */

import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import styles from './Toast.module.css'

export type ToastSeverity = 'success' | 'error' | 'info'

interface ToastEntry {
  id: number
  message: string
  severity: ToastSeverity
}

interface ToastContextValue {
  showToast: (message: string, severity?: ToastSeverity) => void
}

const ToastContext = createContext<ToastContextValue | null>(null)

let _nextId = 0
const AUTO_DISMISS_MS = 5_000

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<ToastEntry[]>([])
  const timersRef = useRef(new Map<number, ReturnType<typeof setTimeout>>())

  const dismiss = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id))
    const timer = timersRef.current.get(id)
    if (timer !== undefined) {
      clearTimeout(timer)
      timersRef.current.delete(id)
    }
  }, [])

  const showToast = useCallback(
    (message: string, severity: ToastSeverity = 'info') => {
      const id = ++_nextId
      setToasts((prev) => [...prev, { id, message, severity }])
      timersRef.current.set(
        id,
        setTimeout(() => dismiss(id), AUTO_DISMISS_MS),
      )
    },
    [dismiss],
  )

  // Clean up all pending timers on unmount.
  useEffect(
    () => () => {
      timersRef.current.forEach((t) => clearTimeout(t))
    },
    [],
  )

  return (
    <ToastContext.Provider value={{ showToast }}>
      {children}
      <div className={styles.container} role="status" aria-live="polite" aria-atomic="false">
        {toasts.map((toast) => (
          <div
            key={toast.id}
            className={[styles.toast, styles[`toast--${toast.severity}`]].join(' ')}
          >
            <span className={styles.message}>{toast.message}</span>
            <button
              className={styles.closeBtn}
              onClick={() => dismiss(toast.id)}
              aria-label="Dismiss notification"
              type="button"
            >
              ×
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext)
  if (!ctx) throw new Error('useToast must be used inside <ToastProvider>')
  return ctx
}
