// SPDX-License-Identifier: AGPL-3.0-or-later

// Package script runs an operator-configured capacity provider: an executable named by a
// `script:<name>` provider in the server's configuration. It depends on the standard library only.
//
// # Wire contract (version 1)
//
// The server starts the configured command once per action. The request is one JSON object on the
// child's standard input, and the child answers with one JSON object on standard output.
//
// A request carries `version` (always 1), `action`, `group` (`id`, `name`, `kind`) and, by action:
//
//   - `wake`: the `members` to power on. Each member names its `worker_id` and, when known, its
//     `hostname`, `ip_address`, `instance_id` and `interfaces` (`name`, `mac`, `addrs`).
//   - `grow`: `desired` and `current`, the member counts the policy wants and the group has.
//   - `retire`: the `members` to remove.
//   - `list`: nothing further. The script reports the instances the group's backend currently runs.
//
// A response carries `result`, `message` and, for `list`, `instance_ids`. `result` is one of:
//
//   - `ok`: the action was carried out (or, for wake, started).
//   - `unsupported`: the script does not implement the action. Not a failure; the caller falls back.
//   - `error`: the action failed. `message` says why.
//
// Rules the runner applies to the child:
//
//   - Empty standard output with exit status 0 is `ok`.
//   - A non-zero exit status is `error`, carrying the exit status and the last line of standard
//     error, whatever standard output held.
//   - Standard output that is not one JSON object, or whose `result` is none of the three values,
//     is `error`.
//   - Standard output is limited to 1 MiB. A script that writes more is killed and the result is
//     `error`.
//   - Standard error is the script's log. It is logged at Info line by line, up to 64 KiB per run,
//     carrying the provider, action and group name; it is never parsed.
//   - The environment is the server's own minus every variable named `SQI_*`, plus the provider's
//     configured `env`. Only the `SQI_` variables are removed: every other variable (PG*, cloud
//     credentials, proxies) is inherited, so an operator who needs the script isolated from them
//     wraps the command (`env -i`, a container, a service account).
//   - Each configured `env` value of four bytes or more is replaced by `<redacted>` wherever the
//     script's own output carries it: in logged standard-error lines, in the last standard-error
//     line quoted in a failure message and in the response `message`. Values are replaced longest
//     first. A value split across the 8 KiB chunks of one very long line is not caught.
//   - A response `message` is cut to 2 KiB on a character boundary and has its control characters
//     (newlines included) replaced by spaces, so a script cannot forge log lines through it.
//   - The run is bounded by the provider's timeout; a script still running at the deadline is
//     killed and [Run] returns an error.
//
// [Run] returns a non-nil error only for a command that cannot start and for a timeout or a
// canceled context; every other failure is an `error` [Response].
package script

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// Version is the request version this package writes.
	Version = 1

	// maxStdout caps the bytes read from a script's standard output.
	maxStdout = 1 << 20
	// maxStderrLogged caps the stderr bytes logged per run.
	maxStderrLogged = 64 << 10
	// maxStderrLine bounds one stderr line held in memory; a longer line is logged in pieces.
	maxStderrLine = 8 << 10
	// waitDelay bounds how long Run waits for a killed script's pipes to close.
	waitDelay = 5 * time.Second
	// defaultTimeout applies when a Provider sets none.
	defaultTimeout = 2 * time.Minute
	// maxMessage caps a Response.Message returned from a script.
	maxMessage = 2 << 10
	// minRedactLen is the shortest env value that is redacted; shorter ones are flags and numbers
	// that would mangle ordinary output.
	minRedactLen = 4
	// redactedText replaces a redacted env value.
	redactedText = "<redacted>"
)

// Provider is one configured script provider.
type Provider struct {
	// Name is the provider's configured name, used only for logging and error messages.
	Name string
	// Command is the executable and its arguments. It is never run through a shell.
	Command []string
	// Env is applied over the server's environment after the SQI_ variables are removed.
	Env map[string]string
	// Timeout bounds one run; zero or negative means two minutes.
	Timeout time.Duration
}

// GroupRef identifies the capacity group an action concerns.
type GroupRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Member is a group member as a script sees it.
type Member struct {
	WorkerID   string         `json:"worker_id"`
	Hostname   string         `json:"hostname,omitempty"`
	IPAddress  string         `json:"ip_address,omitempty"`
	InstanceID string         `json:"instance_id,omitempty"`
	Interfaces []NetInterface `json:"interfaces,omitempty"`
}

// NetInterface is this package's own copy of protocol.NetInterface's JSON shape, so the package
// stays stdlib-only.
type NetInterface struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac"`
	Addrs []string `json:"addrs,omitempty"`
}

// Request is the JSON object a script reads on standard input.
type Request struct {
	Version int      `json:"version"`
	Action  string   `json:"action"` // "wake", "grow", "retire" or "list"
	Group   GroupRef `json:"group"`
	Desired int      `json:"desired,omitempty"` // grow
	Current int      `json:"current,omitempty"` // grow
	Members []Member `json:"members,omitempty"` // wake, retire
}

// Response is the JSON object a script writes on standard output.
type Response struct {
	Result      string   `json:"result"` // "ok", "unsupported" or "error"
	Message     string   `json:"message,omitempty"`
	InstanceIDs []string `json:"instance_ids,omitempty"` // list
}

// Run executes the provider for one request. It returns a non-nil error only when the command
// cannot start, the run times out, or ctx ends; any other failure is a Response with Result
// "error". A nil logger discards the script's standard error.
func Run(ctx context.Context, p Provider, req Request, logger *slog.Logger) (Response, error) {
	if len(p.Command) == 0 {
		return Response{}, fmt.Errorf("provider %q has no command", p.Name)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("provider %q: encode request: %w", p.Name, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	redact := newRedactor(p.Env)
	stdout := &capWriter{limit: maxStdout, onOverflow: cancel}
	stderr := &lineLogger{
		ctx:    ctx,
		redact: redact,
		logger: logger.With("provider", p.Name, "action", req.Action, "group", req.Group.Name),
		budget: maxStderrLogged,
	}

	cmd := exec.CommandContext(runCtx, p.Command[0], p.Command[1:]...) //nolint:gosec // the command is operator configuration
	cmd.Env = buildEnv(p.Env)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	err = cmd.Run()
	stderr.flush()
	if err != nil && cmd.Process == nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, fmt.Errorf("provider %q: %w", p.Name, ctxErr)
		}
		return Response{}, fmt.Errorf("provider %q could not start: %w", p.Name, err)
	}

	// A parent that ended, or a deadline that hit, matters only when it cut the child short.
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, fmt.Errorf("provider %q: %w", p.Name, ctxErr)
		}
	}
	if stdout.overflowed() {
		return errorResponse("standard output exceeded the 1 MiB limit"), nil
	}
	if err != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return Response{}, fmt.Errorf("provider %q timed out after %s: %w", p.Name, timeout, context.DeadlineExceeded)
	}
	resp := interpret(p, redact, err, stdout.bytes(), stderr.last())
	resp.Message = tidyMessage(resp.Message)
	return resp, nil
}

// interpret turns a finished run into a Response.
func interpret(p Provider, redact *redactor, runErr error, out []byte, lastStderr string) Response {
	if runErr != nil && !errors.Is(runErr, exec.ErrWaitDelay) {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return errorResponse("provider run failed: " + runErr.Error())
		}
		msg := fmt.Sprintf("provider %q failed: %s", p.Name, exitErr.Error())
		if lastStderr != "" {
			msg += ": " + truncate(lastStderr, 512) // lastStderr is already redacted
		}
		return errorResponse(msg)
	}

	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return Response{Result: "ok"}
	}
	var resp Response
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return errorResponse("standard output is not a JSON response: " + err.Error())
	}
	switch resp.Result {
	case "ok", "unsupported", "error":
		resp.Message = redact.apply(resp.Message)
		return resp
	default:
		return errorResponse(fmt.Sprintf("unknown result %q in the response", truncate(redact.apply(resp.Result), 64)))
	}
}

func errorResponse(msg string) Response {
	return Response{Result: "error", Message: msg}
}

// truncate cuts s to at most n bytes without splitting a multi-byte character and marks the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// tidyMessage replaces control characters with spaces and caps the length, so a script's message
// cannot forge log lines or flood a caller.
func tidyMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return truncate(s, maxMessage)
}

// redactor replaces provider env values in text the script produced.
type redactor struct {
	r *strings.Replacer
}

// newRedactor covers every env value of minRedactLen bytes or more. Longest values come first, so a
// value that contains another is replaced whole rather than leaving a fragment.
func newRedactor(env map[string]string) *redactor {
	seen := make(map[string]bool, len(env))
	var values []string
	for _, v := range env {
		if len(v) >= minRedactLen && !seen[v] {
			seen[v] = true
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return &redactor{}
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
	pairs := make([]string, 0, 2*len(values))
	for _, v := range values {
		pairs = append(pairs, v, redactedText)
	}
	return &redactor{r: strings.NewReplacer(pairs...)}
}

func (d *redactor) apply(s string) string {
	if d == nil || d.r == nil {
		return s
	}
	return d.r.Replace(s)
}

// buildEnv returns the server's environment without SQI_ variables, with extra applied on top.
// Windows treats variable names case-insensitively, so there the match and the override fold case.
func buildEnv(extra map[string]string) []string {
	fold := func(s string) string { return s }
	if runtime.GOOS == "windows" {
		fold = strings.ToUpper
	}
	overridden := make(map[string]bool, len(extra))
	for k := range extra {
		overridden[fold(k)] = true
	}

	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		name := kv
		// Windows keeps drive-cwd entries such as "=C:=C:\dir"; their name starts at the second byte.
		if i := strings.IndexByte(kv[min(1, len(kv)):], '='); i >= 0 {
			name = kv[:i+min(1, len(kv))]
		}
		folded := fold(name)
		if strings.HasPrefix(folded, "SQI_") || overridden[folded] {
			continue
		}
		env = append(env, kv)
	}

	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

// capWriter keeps up to limit bytes and discards the rest. The first byte past the limit calls
// onOverflow, which kills the script, so a flood cannot hold the run until the timeout. Write never
// fails, so the copy goroutine keeps draining the pipe and the script cannot block on a full one.
type capWriter struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	limit      int
	over       bool
	onOverflow func()
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.over {
		return len(p), nil
	}
	room := w.limit - w.buf.Len()
	if len(p) <= room {
		w.buf.Write(p)
		return len(p), nil
	}
	w.buf.Write(p[:room])
	w.over = true
	if w.onOverflow != nil {
		w.onOverflow()
	}
	return len(p), nil
}

func (w *capWriter) overflowed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.over
}

func (w *capWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Bytes()
}

// lineLogger logs a script's standard error one line at a time, up to a byte budget, and
// remembers the last non-empty line for the failure message.
type lineLogger struct {
	mu        sync.Mutex
	redact    *redactor
	ctx       context.Context //nolint:containedctx // scoped to one Run; only passed to the logger
	logger    *slog.Logger
	budget    int
	truncated bool
	partial   []byte
	lastLine  string
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			l.emit()
			continue
		}
		l.partial = append(l.partial, b)
		if len(l.partial) >= maxStderrLine {
			l.emit()
		}
	}
	return len(p), nil
}

// flush emits a final line that had no newline.
func (l *lineLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit()
}

func (l *lineLogger) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastLine
}

// emit logs and clears the held line. The caller holds l.mu.
func (l *lineLogger) emit() {
	// A value split across two chunks of an over-long line is not seen whole and is not caught.
	line := l.redact.apply(strings.TrimRight(string(l.partial), "\r"))
	l.partial = l.partial[:0]
	if strings.TrimSpace(line) == "" {
		return
	}
	l.lastLine = line
	if l.truncated {
		return
	}
	if len(line) > l.budget {
		l.truncated = true
		l.logger.InfoContext(l.ctx, "provider stderr truncated", "limit_bytes", maxStderrLogged)
		return
	}
	l.budget -= len(line)
	l.logger.InfoContext(l.ctx, "provider stderr", "line", line)
}
