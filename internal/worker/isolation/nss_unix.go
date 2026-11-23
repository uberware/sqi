// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// nssCommandTimeout bounds every id(1)/getent(1) invocation. A hung sssd or
// LDAP backend must not wedge session creation forever.
const nssCommandTimeout = 5 * time.Second

// nssOutputCap bounds how much stdout runNSSCommand will buffer from
// id(1)/getent(1). A well-behaved NSS backend answers in well under a
// kilobyte; the cap stops a misbehaving backend from growing the buffer
// without bound and exhausting worker memory.
const nssOutputCap = 4096

// nssStderrCap bounds how much stderr runNSSCommand will capture from a
// failing id(1)/getent(1) invocation, for inclusion in the returned error.
// As with nssOutputCap, real tool error text is well under a kilobyte; the
// cap stops a misbehaving backend from buffering without bound.
const nssStderrCap = 4096

// cmdRunner executes name with args and returns its captured stdout. It exists
// so tests can inject canned id(1)/getent(1) output instead of depending on a
// real NSS-backed account, which no hermetic test environment has.
type cmdRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// nssToolCandidates lists the absolute paths runNSSCommand tries for name, in
// preference order, before ever falling back to a PATH search. Capable()
// requires euid 0, so this always executes as root; resolving a bare "id" /
// "getent" against the daemon's $PATH would let a trojan binary earlier on
// that PATH answer for the real tool, and its stdout is the uid/gid data the
// credential is built from. Two paths per tool cover the common Linux split
// (Debian/Ubuntu's /usr/bin vs. older/other distros' /bin, sometimes a
// symlink to the other) without hardcoding a single distro's layout.
func nssToolCandidates(name string) []string {
	switch name {
	case "id":
		return []string{"/usr/bin/id", "/bin/id"}
	case "getent":
		return []string{"/usr/bin/getent", "/bin/getent"}
	default:
		return nil
	}
}

// resolveToolPath returns the first candidate that exists as a regular file,
// falling back to exec.LookPath(name) when none do (e.g. a nonstandard OS
// layout), and finally to the bare name so exec fails with its usual
// "executable file not found" error. Falling back to a $PATH search is
// logged at WARN, not Debug: Capable() requires euid 0, so this runs as root,
// and using whichever "id"/"getent" resolves first on the root daemon's $PATH
// is the trojan-binary exposure the absolute candidate list exists to avoid,
// which an operator needs to see.
func resolveToolPath(ctx context.Context, logger *slog.Logger, name string, candidates []string) string {
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	logger.WarnContext(ctx, "isolation: no absolute NSS-aware tool path found, falling back to a PATH search",
		"tool", name, "candidates", candidates)
	if resolved, err := exec.LookPath(name); err == nil {
		return resolved
	}
	return name
}

// newNSSCmd builds the exec.Cmd runNSSCommand runs: resolved to an absolute
// path, bounded by nssCommandTimeout for both the context deadline and
// WaitDelay. WaitDelay closes a gap plain context cancellation leaves open:
// context.WithTimeout kills the process on schedule, but exec.Cmd.Output/Run
// still waits on the stdout pipe until every holder of the write end closes
// it, so a grandchild that inherited the descriptor (common with a
// misbehaving NSS backend spawning helpers) can hold the call well past the
// 5s bound. WaitDelay makes Wait give up and close the pipes itself that long
// after the process has been signaled. It is a separate function so the
// timeout wiring is unit-testable without executing anything.
func newNSSCmd(ctx context.Context, logger *slog.Logger, name string, args ...string) *exec.Cmd {
	path := resolveToolPath(ctx, logger, name, nssToolCandidates(name))
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = nssCommandTimeout
	return cmd
}

// capLimitedWriter is an io.Writer that errors instead of writing once limit
// bytes have been accepted, rather than truncating: a truncated but still
// parseable uid/gid line would be worse than a visible error.
type capLimitedWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *capLimitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.limit {
		return 0, fmt.Errorf("isolation: NSS command output exceeded %d bytes", w.limit)
	}
	return w.buf.Write(p)
}

// runNSSCommand is the production cmdRunner, closed over a logger so it can
// record which absolute path it resolved id(1)/getent(1) to (see
// nssCommandRunner). args are passed to exec.Command as separate argv
// elements, never through a shell, so a queue-supplied username is never
// interpreted as shell syntax. Callers are responsible for validating any
// argument that came from queue configuration before it reaches here;
// validateAccountArg is that gate.
//
// Stdout and stderr are each captured into their own capped buffer rather
// than using cmd.Output(), which would only give an *exec.ExitError room for
// stderr via its own field. Capturing stderr explicitly and folding it into
// the returned error means a failing getent/id still tells an operator why;
// a bare cmd.Run() with Cmd.Stderr nil would drop the tool's error text from
// the log.
func runNSSCommand(ctx context.Context, logger *slog.Logger, name string, args ...string) ([]byte, error) {
	cmd := newNSSCmd(ctx, logger, name, args...)
	logger.DebugContext(ctx, "isolation: resolved NSS-aware tool path",
		"tool", name, "path", cmd.Path)

	var stdout, stderr capLimitedWriter
	stdout.limit = nssOutputCap
	stderr.limit = nssStderrCap
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(stderr.buf.String()); s != "" {
			return nil, fmt.Errorf("isolation: run %s %s: %w (stderr: %s)", cmd.Path, strings.Join(args, " "), err, s)
		}
		return nil, fmt.Errorf("isolation: run %s %s: %w", cmd.Path, strings.Join(args, " "), err)
	}
	return stdout.buf.Bytes(), nil
}

// nssCommandRunner returns the production cmdRunner, closing over logger so
// runNSSCommand can log which tool path it resolved without cmdRunner's
// signature (fixed by the tests that inject a fakeRunner in its place)
// having to carry a logger parameter.
func nssCommandRunner(logger *slog.Logger) cmdRunner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return runNSSCommand(ctx, logger, name, args...)
	}
}

// parseGroupList parses the space-separated gid list id(1) prints for
// `id -G <user>` (e.g. "1001 2001 2010\n"), including the single-gid case
// ("1001\n") produced by an account with no supplementary groups. An
// empty result is a valid parse here; whether it is plausible is for the
// caller to judge (see groupsViaNSS), which knows whether
// empty output is expected or a sign the tool misbehaved.
func parseGroupList(out string) ([]uint32, error) {
	fields := strings.Fields(out)
	groups := make([]uint32, 0, len(fields))
	for _, f := range fields {
		gid, err := parseID(f, "id -G field")
		if err != nil {
			return nil, err
		}
		groups = append(groups, gid)
	}
	return groups, nil
}

// parseGetentPasswdHome extracts field 6 (the home directory) from a
// `getent passwd <user>` line: name:passwd:uid:gid:gecos:home:shell. It
// verifies field 0 names wantUser, the same NSS-aliasing check
// parseGetentGroupGID makes: a misbehaving or aliasing NSS
// backend answering a lookup for one name with an entry for another. The
// comparison is case-insensitive (strings.EqualFold): AD and many LDAP
// backends canonicalize the name they return, so `getent passwd RenderSvc`
// can answer with "rendersvc:...", which a byte-exact comparison would refuse
// as a name mismatch on a correctly configured directory-backed farm.
func parseGetentPasswdHome(out, wantUser string) (string, bool) {
	fields := getentFields(out)
	if len(fields) < 6 || !strings.EqualFold(fields[0], wantUser) {
		return "", false
	}
	return fields[5], true
}

// parseGetentGroupGID extracts field 3 (the gid) from a `getent group
// <name>` line: name:passwd:gid:members. It verifies field 0 names
// wantGroup; without this, a getent backend answering with an entry for a
// different group than the one requested would hand back that group's gid,
// which the caller would then treat as the requested group's identity
// (including running the requested group's non-privileged membership/name
// checks against the wrong group's data). The comparison is case-insensitive
// (strings.EqualFold) for the same reason as parseGetentPasswdHome: AD/LDAP
// backends are commonly case-insensitive and canonicalize the name on
// return, so `getent group Renderers` can answer "renderers:x:3000:", which
// a byte-exact comparison would refuse as a name mismatch.
func parseGetentGroupGID(out, wantGroup string) (uint32, bool) {
	fields := getentFields(out)
	if len(fields) < 3 || !strings.EqualFold(fields[0], wantGroup) {
		return 0, false
	}
	gid, err := parseID(fields[2], "getent group gid")
	if err != nil {
		return 0, false
	}
	return gid, true
}

// getentFields splits the first line of getent(1) output on ':'. Only the
// first line is used: a name lookup returns at most one entry, and dropping
// anything after a trailing newline handles that terminator.
func getentFields(out string) []string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return strings.Split(line, ":")
}
