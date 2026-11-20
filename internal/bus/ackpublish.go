// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"errors"

	"github.com/nats-io/nats.go"
)

// PermanentPublishError reports whether err, from a worker's acknowledged
// publish, cannot be cured by publishing the same message again: it is over
// the broker's payload limit, or the connection closed for good. A worker's
// status and log publishers drop such a message rather than retry it.
func PermanentPublishError(err error) bool {
	return errors.Is(err, nats.ErrMaxPayload) || errors.Is(err, nats.ErrConnectionClosed)
}

// Reconnecting reports whether nc is reconnecting, or making its first
// connection. A worker's acknowledged publishers send nothing then and retry
// later, so their retries do not pile up in the client's reconnect buffer and
// go out together, stale, when the connection comes back. The check and the
// publish are not atomic: a publish written just before the client notices a
// drop is still buffered and sent on reconnect, and the stream drops the
// later retries as duplicates of its message ID.
func Reconnecting(nc *nats.Conn) bool {
	st := nc.Status()
	return st == nats.RECONNECTING || st == nats.CONNECTING
}
