// SPDX-License-Identifier: AGPL-3.0-only

import type { ConnectionState, Envelope, MessageHandler } from './types'

export type { ConnectionState, Envelope, MessageHandler }

const BASE_DELAY_MS = 1_000
const MAX_DELAY_MS = 30_000
const MAX_ATTEMPTS = 10

function isEnvelope(v: unknown): v is Envelope {
  if (typeof v !== 'object' || v === null) return false
  const obj = v as Record<string, unknown>
  return typeof obj['type'] === 'string' && typeof obj['seq'] === 'number'
}

// WebSocketClient manages a persistent WebSocket connection to /api/v1/ws.
// It reconnects automatically with exponential backoff and dispatches incoming
// push messages to registered subject handlers.
export class WebSocketClient {
  private readonly url: string
  private ws: WebSocket | null = null
  private state: ConnectionState = 'disconnected'
  private stateListeners = new Set<(state: ConnectionState) => void>()
  private handlers = new Map<string, Set<MessageHandler>>()
  private seq = 0
  private reconnectAttempt = 0
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private stopped = false

  constructor(url: string) {
    this.url = url
  }

  connect(): void {
    this.stopped = false
    this.reconnectAttempt = 0
    this._clearTimer()
    if (this.ws !== null) {
      this.ws.onclose = null
      this.ws.close()
    }
    this._openSocket()
  }

  disconnect(): void {
    this.stopped = true
    this._clearTimer()
    if (this.ws !== null) {
      this.ws.onclose = null
      this.ws.close()
      this.ws = null
    }
    this._setState('disconnected')
  }

  subscribe(subject: string, handler: MessageHandler): void {
    let set = this.handlers.get(subject)
    if (!set) {
      set = new Set()
      this.handlers.set(subject, set)
      this._sendSubscribe(subject)
    }
    set.add(handler)
  }

  unsubscribe(subject: string, handler: MessageHandler): void {
    const set = this.handlers.get(subject)
    if (!set) return
    set.delete(handler)
    if (set.size === 0) {
      this.handlers.delete(subject)
      this._sendUnsubscribe(subject)
    }
  }

  getState(): ConnectionState {
    return this.state
  }

  onStateChange(listener: (state: ConnectionState) => void): () => void {
    this.stateListeners.add(listener)
    return () => {
      this.stateListeners.delete(listener)
    }
  }

  private _openSocket(): void {
    this._setState('connecting')
    const ws = new WebSocket(this.url)
    this.ws = ws

    ws.onopen = () => {
      this.reconnectAttempt = 0
      this._setState('connected')
      for (const subject of this.handlers.keys()) {
        this._sendSubscribe(subject)
      }
    }

    ws.onmessage = (event: MessageEvent) => {
      this._handleMessage(event.data as string)
    }

    // onerror is always followed by onclose; let onclose drive the reconnect.
    ws.onerror = () => {}

    ws.onclose = () => {
      if (this.stopped) return
      this._scheduleReconnect()
    }
  }

  private _scheduleReconnect(): void {
    if (this.reconnectAttempt >= MAX_ATTEMPTS) {
      this._setState('failed')
      return
    }
    this._setState('reconnecting')
    const delay = Math.min(BASE_DELAY_MS * 2 ** this.reconnectAttempt, MAX_DELAY_MS)
    this.reconnectAttempt++
    this.reconnectTimer = setTimeout(() => {
      this._openSocket()
    }, delay)
  }

  private _clearTimer(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
  }

  private _handleMessage(raw: string): void {
    let parsed: unknown
    try {
      parsed = JSON.parse(raw)
    } catch {
      console.error('[sqi/ws] failed to parse message:', raw)
      return
    }
    if (!isEnvelope(parsed)) {
      console.error('[sqi/ws] invalid envelope shape:', raw)
      return
    }
    if (parsed.type === 'error') {
      console.error('[sqi/ws] server error:', parsed.payload)
      return
    }
    if (parsed.type !== 'push' || !parsed.subject) return
    const handlers = this.handlers.get(parsed.subject)
    handlers?.forEach((h) => h(parsed.payload, parsed))
  }

  private _send(msg: object): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(msg))
    }
  }

  private _sendSubscribe(subject: string): void {
    this._send({ type: 'subscribe', subject, seq: ++this.seq, payload: { since_seq: 0 } })
  }

  private _sendUnsubscribe(subject: string): void {
    this._send({ type: 'unsubscribe', subject, seq: ++this.seq })
  }

  private _setState(state: ConnectionState): void {
    if (this.state === state) return
    this.state = state
    this.stateListeners.forEach((l) => l(state))
  }
}
