// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/uberware/sqi/test/stubproc/record"
)

// stdinLimit bounds what is recorded from stdin. A stub that buffered an
// unbounded pipe would be a way to hang a test rather than a way to observe one.
const stdinLimit = 64 << 10

func main() {
	name := filepath.Base(os.Args[0])
	// Windows: the worker invokes "Render.exe"; the preset and every assertion
	// speak of "Render". Strip the extension so a record is comparable across
	// platforms.
	name = strings.TrimSuffix(name, filepath.Ext(name))

	if err := run(name); err != nil {
		fmt.Fprintf(os.Stderr, "stubproc: %v\n", err)
		os.Exit(70) // EX_SOFTWARE: a stub failure is never a task's own failure
	}
	os.Exit(exitCode(name))
}

// run performs every observable side effect: record, emit, touch, sleep, print.
func run(name string) error {
	if err := applyEnvFromArg(); err != nil {
		return err
	}
	if err := writeRecord(name); err != nil {
		return err
	}
	if os.Getenv("SQI_STUB_PROGRESS") != "" {
		fmt.Printf("openjd_status: %s starting\n", name)
		for _, pct := range []int{25, 50, 75, 100} {
			fmt.Printf("openjd_progress: %d\n", pct)
		}
	}
	if err := touchFromArg(); err != nil {
		return err
	}
	if v := os.Getenv("SQI_STUB_SLEEP"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("SQI_STUB_SLEEP %q: %w", v, err)
		}
		time.Sleep(d)
	}
	// SQI_STUB_STDOUT prints one arbitrary line whatever the exit status, so a
	// test can model a renderer that logs an error and still exits 0 (aerender).
	if v, ok := os.LookupEnv("SQI_STUB_STDOUT"); ok {
		fmt.Println(v)
	}
	return nil
}

// applyEnvFromArg lets one invocation configure itself, so a single worker
// environment can serve jobs that each want the stub to behave differently.
// SQI_STUB_ENV_FROM_ARG names a flag; when the flag appears in argv, the
// argument after it names a file whose sibling "<value>.stubenv" holds
// SQI_STUB_* settings, one KEY=VALUE per line, that override the environment's
// for this invocation. A missing sidecar changes nothing. Only SQI_STUB_ keys
// are accepted, so a sidecar cannot reach anything but the stub.
func applyEnvFromArg() error {
	flag := os.Getenv("SQI_STUB_ENV_FROM_ARG")
	if flag == "" {
		return nil
	}
	args := os.Args[1:]
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return nil
	}
	path := args[i+1] + ".stubenv"
	data, err := os.ReadFile(path) //nolint:gosec // G304: a test stub deliberately reads beside the file its caller names
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("SQI_STUB_ENV_FROM_ARG: %w", err)
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(key, "SQI_STUB_") {
			return fmt.Errorf("%s: %q is not an SQI_STUB_ KEY=VALUE line", path, line)
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// writeRecord appends one JSON line describing this invocation.
func writeRecord(name string) error {
	path := os.Getenv("SQI_STUB_RECORD")
	if path == "" {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	rec := record.Record{
		Command: name,
		Args:    append([]string(nil), os.Args[1:]...),
		Cwd:     cwd,
		Stdin:   readStdin(),
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	// O_APPEND with a single Write of one line under the platform's atomic
	// append: tasks run concurrently, and interleaved half-lines would be
	// undecodable.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: a test stub deliberately writes wherever its caller points it (SQI_STUB_RECORD)
	if err != nil {
		return fmt.Errorf("open record: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	return nil
}

// readStdin returns up to stdinLimit bytes of stdin, or "" when stdin is not a
// pipe. A CLI driven by piped commands has no argv worth snapshotting, so this
// is the only surface such a preset offers.
func readStdin() string {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, stdinLimit))
	if err != nil {
		return ""
	}
	return string(data)
}

// touchFromArg creates the file named by the argument index in
// SQI_STUB_TOUCH_ARG, so a downstream step or stage-out finds real files.
func touchFromArg() error {
	v := os.Getenv("SQI_STUB_TOUCH_ARG")
	if v == "" {
		return nil
	}
	idx, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("SQI_STUB_TOUCH_ARG %q: %w", v, err)
	}
	args := os.Args[1:]
	if idx < 0 || idx >= len(args) {
		return fmt.Errorf("SQI_STUB_TOUCH_ARG %d: only %d arguments", idx, len(args))
	}
	target := args[idx]
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // G301: 0o755 on a test stub's output directory, which holds nothing secret
		return fmt.Errorf("mkdir for %s: %w", target, err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: a test stub deliberately writes wherever its caller points it (an argv index named by SQI_STUB_TOUCH_ARG)
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	defer f.Close()
	if _, err := f.WriteString("stubproc\n"); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

// exitCode resolves the requested exit status: SQI_STUB_EXIT_ON wins over
// SQI_STUB_EXIT when it names this invocation.
func exitCode(name string) int {
	if spec := os.Getenv("SQI_STUB_EXIT_ON"); spec != "" {
		if want, code, ok := parseExitOn(spec); ok && want == name {
			return code
		}
		return 0
	}
	if v := os.Getenv("SQI_STUB_EXIT"); v != "" {
		if code, err := strconv.Atoi(v); err == nil {
			return code
		}
	}
	return 0
}

// parseExitOn parses "<name>=<code>".
func parseExitOn(spec string) (name string, code int, ok bool) {
	name, codeStr, found := strings.Cut(spec, "=")
	if !found {
		return "", 0, false
	}
	code, err := strconv.Atoi(codeStr)
	if err != nil {
		return "", 0, false
	}
	return name, code, true
}
