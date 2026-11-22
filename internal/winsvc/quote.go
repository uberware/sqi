// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

import "strings"

// PowerShellLiteral is s as a PowerShell single-quoted string, with every
// quote that would end one, the typographic single quotes included, doubled.
func PowerShellLiteral(s string) string {
	var sb strings.Builder
	sb.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '\u2018', '\u2019', '\u201a', '\u201b':
			sb.WriteRune(r)
		}
		sb.WriteRune(r)
	}
	sb.WriteByte('\'')
	return sb.String()
}
