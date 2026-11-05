// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"slices"
	"strings"
)

// ClassArgPrefix starts the onRun argument that names a job's class. The driver
// appends it to every template it builds, so a simulated worker can tell which
// class an assignment belongs to without a job lookup (the server sends a job's
// name only with EXPR assignments). A real worker's no-op command ignores it.
const ClassArgPrefix = "--sqi-load-class="

// ClassArg returns the onRun argument naming class.
func ClassArg(class string) string { return ClassArgPrefix + class }

// ClassFromArgs returns the class named by the last class argument in args.
func ClassFromArgs(args []string) (string, bool) {
	for _, arg := range slices.Backward(args) {
		if class, ok := strings.CutPrefix(arg, ClassArgPrefix); ok {
			return class, true
		}
	}
	return "", false
}
