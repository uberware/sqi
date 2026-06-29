// SPDX-License-Identifier: AGPL-3.0-or-later

// Wire types mirroring the server's internal/ws/message.go definitions.

/** Discriminator for the `type` field of a WebSocket {@link Envelope}. */
export type MessageType = 'subscribe' | 'unsubscribe' | 'ping' | 'push' | 'ack' | 'error' | 'pong'

/**
 * Lifecycle state of the {@link WebSocketClient} connection, surfaced to the UI
 * (e.g. by `ConnectionStatusBadge`).
 */
export type ConnectionState =
  'connecting' | 'connected' | 'reconnecting' | 'failed' | 'disconnected'

/** Common message envelope exchanged over the WebSocket in both directions. */
export interface Envelope {
  type: MessageType
  /** Topic the message concerns; present on `subscribe`/`unsubscribe`/`push`. */
  subject?: string
  payload?: unknown
  /** Monotonic sequence number (client-assigned on send, server-assigned on push). */
  seq: number
}

/** Payload of a `subscribe` message; replays pushes after `since_seq` when set. */
export interface SubscribePayload {
  since_seq?: number
}

/** Payload of an `ack` message acknowledging a client message by its sequence. */
export interface AckPayload {
  client_seq: number
  error?: string
}

/** Payload of an `error` message describing a server-side failure. */
export interface ErrorPayload {
  code: string
  message: string
}

/** Callback invoked for each `push` message matching a subscribed subject. */
export type MessageHandler = (payload: unknown, envelope: Envelope) => void
