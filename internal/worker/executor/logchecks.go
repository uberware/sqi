// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// maxReasonLineBytes is how much of a matching line a failOn failure reason
// quotes. The line is already in the task log; the reason only has to identify it.
const maxReasonLineBytes = 200

// redactedEnvDirective is the OpenJD directive that defines a redacted
// environment variable. Its line carries the secret in the clear (nothing has
// registered the value for scrubbing yet when the line is read), so the checker
// never scans it: a failOn pattern that happened to match it would otherwise put
// the secret in the failure reason.
const redactedEnvDirective = "openjd_redacted_env:"

// logChecker enforces one task's SQI_LOG_CHECKS: it observes every line the
// onRun process writes and, once the process has exited, gives a verdict.
//
// observe is called from the stdout and stderr readers concurrently. The
// compiled patterns are immutable, a requireLine match is a lock-free flag, and
// only the first failOn match takes the lock.
type logChecker struct {
	failOn  []*regexp.Regexp
	require []*regexp.Regexp
	// seen[i] is set once some line has matched require[i].
	seen []atomic.Bool

	mu         sync.Mutex
	failReason string
	failed     bool
}

// newLogChecker compiles c. A nil c means the step declared no checks and
// yields (nil, nil); every method of a nil *logChecker is a no-op, so callers
// need no nil guard.
func newLogChecker(c *protocol.LogChecks) (*logChecker, error) {
	if c == nil {
		return nil, nil
	}
	failOn, err := compilePatterns("failOn", c.FailOn)
	if err != nil {
		return nil, err
	}
	require, err := compilePatterns("requireLine", c.RequireLine)
	if err != nil {
		return nil, err
	}
	return &logChecker{failOn: failOn, require: require, seen: make([]atomic.Bool, len(require))}, nil
}

func compilePatterns(list string, patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", list, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// observe checks one scrubbed output line. It is safe for concurrent use.
func (c *logChecker) observe(line string) {
	if c == nil || strings.HasPrefix(strings.TrimSpace(line), redactedEnvDirective) {
		return
	}
	for _, re := range c.failOn {
		if re.MatchString(line) {
			c.recordFailure(re.String(), line)
			break
		}
	}
	for i, re := range c.require {
		if !c.seen[i].Load() && re.MatchString(line) {
			c.seen[i].Store(true)
		}
	}
}

// recordFailure keeps the first failOn match; later ones are ignored.
func (c *logChecker) recordFailure(pattern, line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return
	}
	c.failed = true
	c.failReason = fmt.Sprintf("log line matched failOn pattern %q: %q", pattern, truncateLine(line))
}

// verdict reports whether the checks failed and why: the first failOn match if
// there was one, else the first requireLine pattern no line matched. Call it
// after the process has exited and its output has been drained.
func (c *logChecker) verdict() (reason string, failed bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	reason, failed = c.failReason, c.failed
	c.mu.Unlock()
	if failed {
		return reason, true
	}
	for i, re := range c.require {
		if !c.seen[i].Load() {
			return fmt.Sprintf("the task never printed a line matching requireLine pattern %q", re.String()), true
		}
	}
	return "", false
}

// wrap returns a handler that feeds every line to the checker before passing it,
// unchanged, to h. A nil checker returns h itself.
func (c *logChecker) wrap(h OutputHandler) OutputHandler {
	if c == nil {
		return h
	}
	return &checkedHandler{checker: c, next: h}
}

type checkedHandler struct {
	checker *logChecker
	next    OutputHandler
}

func (h *checkedHandler) HandleLine(ctx context.Context, taskID, attemptID, sessionID, stream, line string) {
	h.checker.observe(line)
	h.next.HandleLine(ctx, taskID, attemptID, sessionID, stream, line)
}

// truncateLine cuts line to at most maxReasonLineBytes bytes, never inside a
// UTF-8 character, and marks the cut with an ellipsis.
func truncateLine(line string) string {
	if len(line) <= maxReasonLineBytes {
		return line
	}
	cut := maxReasonLineBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "…"
}
