// SPDX-License-Identifier: AGPL-3.0-only
/* eslint-disable react-refresh/only-export-components -- context files export both provider and hooks */

import { createContext, useContext, useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { WebSocketClient } from './client'
import type { ConnectionState, MessageHandler } from './types'

export type { ConnectionState, MessageHandler }

function buildWsUrl(): string {
  const { protocol, host } = window.location
  const wsProtocol = protocol === 'https:' ? 'wss:' : 'ws:'
  return `${wsProtocol}//${host}/api/v1/ws`
}

interface WsContextValue {
  client: WebSocketClient
  state: ConnectionState
}

const WsContext = createContext<WsContextValue | null>(null)

function useWsContext(): WsContextValue {
  const ctx = useContext(WsContext)
  if (!ctx) throw new Error('WebSocket hooks must be used inside WebSocketProvider')
  return ctx
}

/**
 * Provides a single {@link WebSocketClient} to the React tree and tracks its
 * connection state. Connects on mount and disconnects on unmount.
 *
 * @param url - Optional explicit WebSocket URL; defaults to `/api/v1/ws` on the
 *   current origin (with `ws`/`wss` chosen from the page protocol).
 */
export function WebSocketProvider({ children, url }: { children: ReactNode; url?: string }) {
  // useState lazy init creates the client exactly once per mount — stable across renders.
  const [client] = useState(() => new WebSocketClient(url ?? buildWsUrl()))
  const [state, setState] = useState<ConnectionState>('disconnected')

  useEffect(() => {
    const off = client.onStateChange(setState)
    client.connect()
    return () => {
      off()
      client.disconnect()
    }
  }, [client])

  return <WsContext.Provider value={{ client, state }}>{children}</WsContext.Provider>
}

/**
 * Subscribe to push messages for `subject` for the lifetime of the calling
 * component. Unsubscribes automatically on unmount or when `subject` changes.
 * The handler reference is stabilised internally, so callers may pass an inline
 * arrow function without causing re-subscription on every render.
 *
 * @param subject - Subject to subscribe to (e.g. `jobs`, `workers`).
 * @param handler - Invoked with the payload and full envelope of each push.
 */
export function useWebSocket(subject: string, handler: MessageHandler): void {
  const { client } = useWsContext()
  const handlerRef = useRef<MessageHandler>(handler)
  useLayoutEffect(() => {
    handlerRef.current = handler
  })

  useEffect(() => {
    const stableHandler: MessageHandler = (payload, envelope) => {
      handlerRef.current(payload, envelope)
    }
    client.subscribe(subject, stableHandler)
    return () => {
      client.unsubscribe(subject, stableHandler)
    }
  }, [client, subject])
}

/** Read the current WebSocket {@link ConnectionState} from context. */
export function useConnectionState(): ConnectionState {
  return useWsContext().state
}
