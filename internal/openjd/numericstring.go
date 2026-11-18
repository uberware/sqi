// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"strconv"

	"github.com/uberware/sqi/internal/openjd/expr"
)

// parseIntString and parseFloatString read an <intstring> or <floatstring>
// (Template Schemas §7.5): the number with any white space around it ignored.
// Every place this package reads a number from a string — a template value, a
// resolved format string, a value given when a Job is created — goes through
// one of them, so all of those places agree on what a number is.
func parseIntString(s string) (int64, error) {
	return strconv.ParseInt(expr.TrimNumericSpace(s), 10, 64)
}

func parseFloatString(s string) (float64, error) {
	return strconv.ParseFloat(expr.TrimNumericSpace(s), 64)
}
