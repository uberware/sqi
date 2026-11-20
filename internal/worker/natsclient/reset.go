// SPDX-License-Identifier: AGPL-3.0-or-later

package natsclient

import "errors"

// isConnReset reports whether err is the broker resetting the connection.
//
// A broker that refuses a credential sends -ERR and closes, but the auth
// callout runs in its read loop, so the client's PING, written after CONNECT,
// can still be unread when it closes: the close then sends a reset, and on
// Windows a reset discards the -ERR the client has not read. A refusal can
// therefore arrive as a reset.
func isConnReset(err error) bool {
	if err == nil {
		return false
	}
	for _, errno := range connResetErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
