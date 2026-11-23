// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import (
	"strconv"
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
// unicode.IsSpace tests. It is not what the isspace() function tests: that one
// is Python's str.isspace and also accepts U+001C–U+001F, which §7.5 names as
// not white space here.
//
// Every package that reads a numeric string uses this one definition, so the
// worker, the server and the expression evaluator agree on what a number is.
func TrimNumericSpace(s string) string {
	return strings.TrimFunc(s, unicode.IsSpace)
}

// ParseIntString reads an <intstring> (Template Schemas §7.5): a base-10
// integer with any white space around it ignored.
func ParseIntString(s string) (int64, error) { return strconv.ParseInt(TrimNumericSpace(s), 10, 64) }

// ParseFloatString reads a <floatstring> (Template Schemas §7.5): a number with
// any white space around it ignored.
func ParseFloatString(s string) (float64, error) { return strconv.ParseFloat(TrimNumericSpace(s), 64) }
