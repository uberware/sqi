// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxStatusTextBytes bounds the status text of a progress snapshot. A task
// controls the text, and it is stored per attempt and fanned out to every
// subscribed browser, so it is cut at the worker and again at the server.
const MaxStatusTextBytes = 1024

// SanitizeStatusText makes task-controlled status text safe to store and
// display: invalid UTF-8 and control characters are dropped (a NUL byte is
// rejected by PostgreSQL text columns, which would fail every snapshot of the
// attempt), tabs and line breaks become spaces, surrounding space is trimmed,
// and the result is cut to [MaxStatusTextBytes] on a rune boundary. The worker
// and the server both apply it, so the server does not depend on the worker
// having done so.
func SanitizeStatusText(s string) string {
	var b strings.Builder
	b.Grow(min(len(s), MaxStatusTextBytes))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
			continue
		case r == '\t' || r == '\n' || r == '\r':
			r = ' '
		case unicode.IsControl(r):
			continue
		}
		if b.Len() == 0 && unicode.IsSpace(r) {
			continue // leading space must not eat into the limit
		}
		if b.Len()+utf8.RuneLen(r) > MaxStatusTextBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimRightFunc(b.String(), unicode.IsSpace)
}
