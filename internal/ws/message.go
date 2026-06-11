// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ws defines the typed WebSocket message envelope shared by the server
// upgrade handler (internal/api) and the fanout layer (tasks 89–91).
//
// Every message exchanged over the /api/v1/ws connection — in both directions
// — is encoded as a JSON [Envelope].  Clients send subscription control
// messages; the server sends push notifications and acknowledgements.
//
// # Wire format
//
// All messages are JSON text frames. Binary frames are not used. The top-level
// structure is always an [Envelope]:
//
//	{
//	  "type":    "subscribe",
//	  "subject": "jobs/abc123/tasks",
//	  "payload": { … },
//	  "seq":     42
//	}
//
// # Sequence numbers
//
//   - Server TypePush messages carry a per-subject monotonically increasing
//     sequence number assigned by the Hub (tasks 89–91).  The sequence is
//     global across connections so clients can store the last received Seq
//     and pass it as since_seq when reconnecting to replay missed events.
//   - Client messages carry a seq chosen by the client. The server echoes it in
//     the corresponding [TypeAck] response so the client can correlate replies.
//   - seq is 0 for messages where sequence tracking is not meaningful (e.g.
//     [TypePing] / [TypePong], [TypeAck], [TypeError]).
package ws

import "encoding/json"

// MessageType is the discriminator field in [Envelope].
type MessageType string

const (
	// Client-to-server message types.

	// TypeSubscribe requests delivery of server-push messages for Subject.
	// Payload must be a [SubscribePayload].
	TypeSubscribe MessageType = "subscribe"

	// TypeUnsubscribe cancels a previously registered subscription for Subject.
	// No payload required.
	TypeUnsubscribe MessageType = "unsubscribe"

	// TypePing is a client-initiated keep-alive.  The server replies with
	// [TypePong].  No payload; seq is 0.
	TypePing MessageType = "ping"

	// Server-to-client message types.

	// TypePush carries a server-initiated event for the subscribed Subject.
	// Payload shape varies by subject (see subject constants).
	TypePush MessageType = "push"

	// TypeAck confirms receipt of a client message. Payload is an [AckPayload]
	// containing the echoed client seq and an optional error detail.
	TypeAck MessageType = "ack"

	// TypeError reports a protocol-level or subscription error to the client.
	// Payload is an [ErrorPayload].
	TypeError MessageType = "error"

	// TypePong is the server reply to [TypePing].  No payload; seq is 0.
	TypePong MessageType = "pong"
)

// Subject constants for the subscription system (tasks 89–91).
// These are prefix patterns; callers substitute resource IDs as needed.
const (
	// SubjectJobs streams aggregate job-summary events for all jobs.
	SubjectJobs = "jobs"

	// SubjectJobTasksFmt streams task-level events for a specific job.
	// Format: "jobs/{job_id}/tasks".
	SubjectJobTasksFmt = "jobs/%s/tasks"

	// SubjectWorkers streams worker status events.
	SubjectWorkers = "workers"

	// SubjectTaskLogsFmt streams log chunks for a specific task attempt.
	// Format: "tasks/{task_id}/logs".
	SubjectTaskLogsFmt = "tasks/%s/logs"
)

// Envelope is the single message type used for all WebSocket communication.
// Every frame sent or received on /api/v1/ws is a JSON-encoded Envelope.
//
// The zero value is not a valid message; Type must always be set.
type Envelope struct {
	// Type is the message discriminator. Required.
	Type MessageType `json:"type"`

	// Subject identifies the resource or stream the message concerns.
	//
	// For client subscribe/unsubscribe: the subscription target
	// (e.g. "jobs/abc123/tasks").
	//
	// For server push: the subject that generated the event.
	//
	// Omitted for ping/pong, ack, and error messages not tied to a subject.
	Subject string `json:"subject,omitempty"`

	// Payload carries message-specific data as a raw JSON value.
	//
	//   TypeSubscribe   → [SubscribePayload]
	//   TypePush        → subject-specific event struct (defined in tasks 89–91)
	//   TypeAck         → [AckPayload]
	//   TypeError       → [ErrorPayload]
	//   TypeUnsubscribe, TypePing, TypePong → nil
	Payload json.RawMessage `json:"payload,omitempty"`

	// Seq is the monotonically increasing sequence number.
	//
	//   TypePush (server → client): a per-subject hub-level counter assigned by
	//   [Hub]. Starts at 1 and increases globally (not per-connection) so
	//   clients can pass the last received Seq as since_seq when reconnecting.
	//
	//   TypeAck (server → client): 0; the acknowledged client seq is carried in
	//   [AckPayload.ClientSeq] instead.
	//
	//   Client messages (TypeSubscribe, TypeUnsubscribe): a client-chosen
	//   counter echoed back in the corresponding TypeAck so the client can
	//   correlate responses.
	//
	//   TypePing / TypePong / TypeError: always 0.
	Seq uint64 `json:"seq"`
}

// SubscribePayload is the payload of a [TypeSubscribe] client message.
type SubscribePayload struct {
	// SinceSeq instructs the server to replay any buffered push messages with
	// seq strictly greater than SinceSeq before streaming live events. A value
	// of 0 (the default) requests only live events going forward (task 91).
	SinceSeq uint64 `json:"since_seq,omitempty"`
}

// AckPayload is the payload of a [TypeAck] server message.
type AckPayload struct {
	// ClientSeq echoes the seq from the client message being acknowledged.
	ClientSeq uint64 `json:"client_seq"`

	// Error is non-empty when the acknowledged request failed.
	// Clients should treat a non-empty Error as equivalent to [TypeError] for
	// the specific operation.
	Error string `json:"error,omitempty"`
}

// ErrorPayload is the payload of a [TypeError] server message.
type ErrorPayload struct {
	// Code is a machine-readable error identifier, e.g. "invalid_subject".
	Code string `json:"code"`

	// Message is a human-readable description of the error.
	Message string `json:"message"`
}
