// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

import "strings"

// QuotePowerShell escapes s for a PowerShell single-quoted string, which also
// ends at the typographic single quotes.
func QuotePowerShell(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '\'', '\u2018', '\u2019', '\u201a', '\u201b':
			sb.WriteRune(r)
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
