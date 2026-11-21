// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"fmt"
	"strings"
)

// streamBreak is a stream of the embedded broker that can no longer store
// messages. nats-server keeps a stream's write error for good, so the stream
// stays broken until the broker restarts.
type streamBreak struct {
	// Stream is the stream's name; "" means JetStream as a whole was shut
	// down for lack of resources.
	Stream string
	// Err is nats-server's error.
	Err string
}

// The nats-server v2.15.3 log formats that report a break, as it passes them
// to its logger. They are matched on the format, not on the formatted text,
// so a change upstream fails TestRecovery_RealDescriptorExhaustion rather
// than silently turning detection off.
const (
	// stream.go setWriteErrLocked: account, stream, error; once per stream.
	breakStreamFormat = "JetStream stream '%s > %s' critical write error: %v"
	// filestore.go fileStore.error formats the stream name into the format:
	// "Filestore [<stream>] Critical write error: %v".
	breakFilestorePrefix = "Filestore ["
	breakFilestoreSuffix = "] Critical write error: %v"
	// jetstream.go handleOutOfSpace: "JetStream out of %s resources, will be
	// DISABLED", or with no store type.
	breakDisabledPrefix = "JetStream out of"
	breakDisabledSuffix = "resources, will be DISABLED"
)

// parseBreak reports whether the line nats-server logs with format and v says
// a stream broke, and which.
func parseBreak(format string, v []any) (streamBreak, bool) {
	switch {
	case format == breakStreamFormat:
		if len(v) < 3 {
			return streamBreak{}, false
		}
		return streamBreak{Stream: fmt.Sprint(v[1]), Err: fmt.Sprint(v[2])}, true
	case strings.HasPrefix(format, breakFilestorePrefix) && strings.HasSuffix(format, breakFilestoreSuffix):
		if len(v) < 1 {
			return streamBreak{}, false
		}
		name := strings.TrimSuffix(strings.TrimPrefix(format, breakFilestorePrefix), breakFilestoreSuffix)
		return streamBreak{Stream: name, Err: fmt.Sprint(v[0])}, true
	case strings.HasPrefix(format, breakDisabledPrefix) && strings.HasSuffix(format, breakDisabledSuffix):
		return streamBreak{Err: fmt.Sprintf(format, v...)}, true
	}
	return streamBreak{}, false
}
