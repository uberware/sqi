// SPDX-License-Identifier: AGPL-3.0-only

// Wire types mirroring the server's internal/ws/message.go definitions.

export type MessageType = 'subscribe' | 'unsubscribe' | 'ping' | 'push' | 'ack' | 'error' | 'pong'

export type ConnectionState =
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'failed'
  | 'disconnected'

export interface Envelope {
  type: MessageType
  subject?: string
  payload?: unknown
  seq: number
}

export interface SubscribePayload {
  since_seq?: number
}

export interface AckPayload {
  client_seq: number
  error?: string
}

export interface ErrorPayload {
  code: string
  message: string
}

export type MessageHandler = (payload: unknown, envelope: Envelope) => void
