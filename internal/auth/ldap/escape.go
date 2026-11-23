// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"strings"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// EscapeUsernameFilter escapes a username for interpolation into a search
// filter. UserFilter is operator-supplied with a %s placeholder, so an
// unescaped `*)(uid=*` would rewrite "(uid=%s)" into a filter matching every
// entry in the base DN, bypassing authentication.
func EscapeUsernameFilter(username string) string {
	return ldapv3.EscapeFilter(username)
}

// dnSpecialChars are the characters escaped in a DN value: RFC 4514's comma,
// plus, double quote, backslash, angle brackets and semicolon, plus equals
// (see EscapeUsernameDN).
const dnSpecialChars = `,+"\<>;=`

// EscapeUsernameDN escapes a username for interpolation into UserDNTemplate.
// Without it a username containing a comma could graft extra RDNs onto the
// template and bind as a different entry.
//
// go-ldap/v3's EscapeDN follows the RFC 4514 grammar, which does not require
// escaping "=" inside a value (only the top-level type/value separator is
// significant to a conformant parser). UserDNTemplate is filled by plain %s
// substitution rather than a structured DN encoder, so this escapes locally
// and treats "=" as special too, in case a consumer of the template is not a
// fully RFC-conformant DN parser.
func EscapeUsernameDN(username string) string {
	if username == "" {
		return ""
	}
	runes := []rune(username)
	var b strings.Builder
	b.Grow(len(username))
	for i, r := range runes {
		switch {
		case r == 0:
			b.WriteString(`\00`)
		case strings.ContainsRune(dnSpecialChars, r):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == ' ' && (i == 0 || i == len(runes)-1):
			b.WriteByte('\\')
			b.WriteRune(r)
		case i == 0 && r == '#':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
