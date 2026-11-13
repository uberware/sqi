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
func ClassFromArgs(args []string) (string, bool) { return LastArg(args, ClassArgPrefix) }

// LastArg returns the value of the last argument in args that starts with
// prefix.
func LastArg(args []string, prefix string) (string, bool) {
	for _, arg := range slices.Backward(args) {
		if v, ok := strings.CutPrefix(arg, prefix); ok {
			return v, true
		}
	}
	return "", false
}

// KeyArgPrefix starts the onRun argument that names a task: the driver puts
// it before the class argument, with the task's parameters as format
// strings, so a real task's no-op command can name what it writes.
const KeyArgPrefix = "--sqi-load-key="

// KeyArg returns the onRun argument naming the task key.
func KeyArg(key string) string { return KeyArgPrefix + key }

// KeyFromArgs returns the value of the last key argument.
func KeyFromArgs(args []string) (string, bool) { return LastArg(args, KeyArgPrefix) }

// TaskKey is the key of one task: "<job name>/<step name>/<Frame>", with
// "/<Block>" added when block is not empty.
func TaskKey(jobName, stepName, frame, block string) string {
	key := jobName + "/" + stepName + "/" + frame
	if block != "" {
		key += "/" + block
	}
	return key
}
