// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strings"
	"unicode"
)

// TrimNumericSpace removes the white space a numeric string may carry around
// its number. Template Schemas §7.5 allows it on every <intstring>,
// <floatstring> and their positive forms, and RFC 0005 §1.2.3 on a string
// converted to int or float, so " 7 " is 7 and "2.5\n" is 2.5. White space
// inside the number is not removed, so "1 000" still fails to parse.
//
// White space is the Unicode White_Space property, which is exactly the set
// unicode.IsSpace tests. It is NOT what the isspace() function tests: that one
// is Python's str.isspace and also accepts U+001C–U+001F, which §7.5 names as
// not white space here.
//
// Every package that reads a numeric string uses this one definition, so the
// worker, the server and the expression evaluator agree on what a number is.
func TrimNumericSpace(s string) string {
	return strings.TrimFunc(s, unicode.IsSpace)
}
