// SPDX-License-Identifier: AGPL-3.0-or-later

// Package staging implements the stage_locally path delivery: copying job
// inputs to worker-local scratch before a task runs and outputs back afterward.
// sqi never copies bytes itself; it invokes an operator-configured sync command
// per path. The scratch destination paths are returned as path-map rules so the
// other deliveries advertise them.
package staging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// Stager copies staged paths via an external sync command, or the built-in
// copy when unconfigured/defaults are enabled.
type Stager struct {
	scratchBase string
	syncCommand string
	defaults    bool
	logger      *slog.Logger
	warnOnce    sync.Once

	// ancestorMode is the mode StageIn creates the shared scratch base and
	// job directory at — 0o750 unless [WithIsolationCapable] was passed at
	// construction. Decided ONCE, for the whole lifetime of this Stager, not
	// per-assignment: see WithIsolationCapable's doc for why.
	ancestorMode os.FileMode
}

// Option configures a [Stager] at construction. See [WithIsolationCapable].
type Option func(*Stager)

// WithIsolationCapable records, once at worker boot, whether this worker is
// capable of run-as-user isolation at all — the same predicate
// cmd/sqi-worker's effectiveSessionRoot already decides the session root's
// mode from (there: isRoot(); here: the caller passes the identical
// boolean). It is NOT "did this particular assignment carry a credential":
// scratchBase and scratchBase/jobID are shared by every attempt of every
// job, including a mix of isolated and non-isolated queues on the same
// worker, so a per-assignment decision would let whichever job type
// happens to run first after a fresh scratch base decide that shared
// directory's mode for every attempt after it — e.g. a non-isolated task
// landing first creates the base at 0750, and every subsequent isolated
// task then fails ValidateTraversable against a base it can never widen
// itself. Deciding the mode once, from a per-worker capability signal,
// makes it deterministic instead of a race against assignment order. A
// worker that is never capable of isolating keeps the pre-isolation 0750
// default (the zero value) whether or not this option is called.
func WithIsolationCapable(capable bool) Option {
	return func(s *Stager) {
		if capable {
			s.ancestorMode = 0o711
		} else {
			s.ancestorMode = 0o750
		}
	}
}

// New returns a Stager. scratchBase/syncCommand come from worker config;
// defaults enables the built-in copy + TEMP scratch when staging is
// otherwise unconfigured. Pass [WithIsolationCapable] so a worker capable of
// run-as-user isolation creates its shared scratch ancestors traversable
// from birth; omitting it keeps the narrower 0750 default.
func New(scratchBase, syncCommand string, defaults bool, logger *slog.Logger, opts ...Option) *Stager {
	s := &Stager{
		scratchBase:  scratchBase,
		syncCommand:  syncCommand,
		defaults:     defaults,
		logger:       logger,
		ancestorMode: 0o750,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

const builtinSentinel = "builtin"

// useBuiltin reports whether the built-in copy handles transfers (no explicit
// shell sync command).
func (s *Stager) useBuiltin() bool {
	return s.syncCommand == "" || s.syncCommand == builtinSentinel
}

// effectiveScratch returns the configured scratch base, or the TEMP default.
func (s *Stager) effectiveScratch() string {
	return EffectiveScratchBase(s.scratchBase)
}

// EffectiveScratchBase returns the scratch directory staging uses:
// scratchDir verbatim if set, otherwise the platform TEMP-based default.
// Exported so cmd/sqi-worker's boot-time isolation ancestor check (see
// isolation.ValidateTraversable) validates the EXACT path StageIn will
// actually use, rather than duplicating this default and risking drift.
func EffectiveScratchBase(scratchDir string) string {
	if scratchDir != "" {
		return scratchDir
	}
	return filepath.Join(os.TempDir(), "sqi-staging")
}

// Configured reports whether staging can proceed: explicitly configured
// (scratch + shell command), or the built-in copy is available (defaults on,
// or the `builtin` sentinel was set explicitly).
func (s *Stager) Configured() bool {
	return s.defaults || s.syncCommand == builtinSentinel ||
		(s.scratchBase != "" && s.syncCommand != "")
}

// StageIn prepares a per-attempt scratch directory for every staged entry and
// returns one path-map rule per entry (original path -> scratch path) plus the
// scratch directory. IN/INOUT inputs are copied into scratch via the sync command;
// OUT entries only get their scratch destination created (the task writes the
// output there, and [Stager.StageOut] copies it back afterward). Returning a rule
// for OUT entries too is what lets the other deliveries redirect the task's
// OUTPUT paths into scratch — without it the task writes to the real path and
// copy-out fails on a missing scratch file. On any failure the partial scratch
// directory is removed.
//
// cred is the run-as-user credential for the task that will read/write these
// paths, or nil when the assignment carries no isolation. Staging always runs
// as the daemon (this is an operator-configured command, not job code — see
// internal/worker/session and internal/worker/executor for the two launch
// sites that DO carry a credential), so every path it writes is daemon-owned
// by default. When cred is non-nil, StageIn chowns the scratch directory to
// cred's uid/gid so the isolated task can read its staged inputs and write
// into its staged output directories; without this the task would see
// permission-denied on its own files.
//
// scratchBase (s.effectiveScratch()) and scratchBase/jobID are shared by every
// attempt of every job — and, across jobs, by every different run-as-user
// identity, including a mix of isolated and non-isolated queues on the same
// worker — so their mode (s.ancestorMode) is decided ONCE, at Stager
// construction (see [WithIsolationCapable]), from whether this WORKER is
// capable of isolating at all, never from whether THIS assignment's cred is
// non-nil: a per-assignment decision would let whichever job type happens to
// run first after a fresh scratch base decide that shared directory's mode
// for every attempt after it, on a coin that re-flips every time the base is
// recreated (e.g. after a reboot when it sits under /tmp). A capable worker
// therefore creates base/jobDir traversable FROM BIRTH (0711) rather than
// narrow and widened after the fact: chowning either to any one identity
// would grant that identity nothing extra (search doesn't require ownership)
// while breaking every other attempt's/user's access, and mutating an
// EXISTING directory's mode is exactly the anti-pattern this codebase's
// isolation split eliminates elsewhere (see workerconfig.LoadOrCreateWorkerID's
// doc). Creating fresh at 0711 is a different, safe operation: os.MkdirAll
// never touches the mode of a directory that already exists, so a
// pre-existing scratch_dir at a narrower mode is caught by cmd/sqi-worker's
// boot-time isolation.ValidateTraversable check (or, for a task that arrives
// before that boot-time check would apply, the per-assignment check just
// below) instead of being silently widened here. A worker that is never
// capable of isolating keeps the pre-isolation 0750 mode instead of gaining a
// needless traversable-by-anyone directory — the same reasoning this
// codebase's other unconditional-widening fix (the session root, see
// workerconfig.LoadOrCreateWorkerID's doc) already applied once, now
// extended to this file. Only the per-attempt leaf (scratchDir itself, via
// ChownRecursive below) is chowned per-assignment, since it belongs to
// exactly this one attempt/identity — chowning is safe per-assignment
// because it is a fresh directory this call alone owns, unlike base/jobDir
// which persist across every other attempt. Go's forkAndExecInChild sets
// process credentials BEFORE chdir, so without both ancestors being
// traversable the isolated task cannot reach its own staged inputs even
// though scratchDir itself is correctly chowned.
func (s *Stager) StageIn(ctx context.Context, jobID, attemptID string, entries []protocol.StageEntry, cred *isolation.Credential) ([]protocol.PathMapRule, string, error) {
	base := s.effectiveScratch()
	jobDir := filepath.Join(base, jobID)
	scratchDir := filepath.Join(jobDir, attemptID)

	// Per-assignment ancestor validation — cred != nil is exactly the moment
	// this attempt actually carries a run-as-user identity. cmd/sqi-worker's
	// boot-time validateIsolationAncestors only runs when isolation.required
	// is set (see its own doc: root and will-actually-isolate are not the same
	// predicate), so every OTHER isolated attempt is validated here instead —
	// the IDENTICAL check with the IDENTICAL actionable message, against the
	// EXACT base this call is about to create — failing only THIS task rather
	// than the whole worker.
	if cred != nil {
		if err := isolation.ValidateTraversable(base); err != nil {
			return nil, "", fmt.Errorf("staging: %w", err)
		}
	}

	if err := os.MkdirAll(base, s.ancestorMode); err != nil {
		return nil, "", fmt.Errorf("staging: create scratch base %q: %w", base, err)
	}
	if err := os.MkdirAll(jobDir, s.ancestorMode); err != nil {
		return nil, "", fmt.Errorf("staging: create job scratch dir %q: %w", jobDir, err)
	}
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		return nil, "", fmt.Errorf("staging: create scratch dir %q: %w", scratchDir, err)
	}
	rules, err := s.prepareEntries(ctx, scratchDir, entries)
	if err != nil {
		s.Cleanup(scratchDir)
		return nil, "", err
	}
	if err := isolation.ChownRecursive(scratchDir, cred); err != nil {
		s.Cleanup(scratchDir)
		return nil, "", fmt.Errorf("staging: chown scratch dir %q to run-as-user: %w", scratchDir, err)
	}
	return rules, scratchDir, nil
}

// prepareEntries iterates entries and, for every staged entry (IN/OUT/INOUT),
// creates a per-index scratch subdirectory (<scratchDir>/<i>/<basename>) and a
// path-map rule so the other deliveries redirect both the task's inputs and
// outputs into scratch. Input bytes (IN/INOUT) are copied in via the sync command;
// OUT entries are produced by the task, so nothing is copied in — only the scratch
// directory is created so the task can write there. The per-index layout (so
// same-basename entries never collide) matches what StageOut uses for copy-back.
// Extracted to keep StageIn under the cyclop complexity limit.
func (s *Stager) prepareEntries(ctx context.Context, scratchDir string, entries []protocol.StageEntry) ([]protocol.PathMapRule, error) {
	var rules []protocol.PathMapRule
	for i, e := range entries {
		if e.Direction != "IN" && e.Direction != "INOUT" && e.Direction != "OUT" {
			continue
		}
		subDir := filepath.Join(scratchDir, strconv.Itoa(i))
		if err := os.MkdirAll(subDir, 0o750); err != nil {
			return nil, fmt.Errorf("staging: create subdir %q: %w", subDir, err)
		}
		dest := filepath.Join(subDir, filepath.Base(e.Path))
		// Copy existing bytes in only for inputs; outputs are produced by the task.
		if e.Direction == "IN" || e.Direction == "INOUT" {
			if err := s.transfer(ctx, e.Path, dest, e.ObjectType); err != nil {
				return nil, fmt.Errorf("staging: copy-in %q: %w", e.Path, err)
			}
		}
		rules = append(rules, protocol.PathMapRule{
			SourcePathFormat: pathmap.DetectSourceFormat(e.Path),
			SourcePath:       e.Path,
			DestinationPath:  dest,
		})
	}
	return rules, nil
}

// StageOut copies every OUT/INOUT entry from scratch back to its original path.
// It iterates the full entries slice with its original index so the per-index
// subdirectory (<scratchDir>/<i>/<basename>) matches what prepareEntries created.
//
// The whole loop runs inside ONE os.Root rooted at scratchDir. Every source is
// opened through it, so containment and reparse-point refusal are enforced by
// the kernel at open time rather than computed from a path beforehand — see
// openStageOutSource for why the previous path-based check was unsound on
// Windows. Validation runs upstream of BOTH the built-in copy and an
// operator-configured sync_command, which is the only place a check can cover
// both identically: sqi cannot audit an arbitrary sync_command template, since
// whether it dereferences symlinks is a property of that command ("rsync -a"
// preserves them; "rsync -aL" or plain "cp" follow them).
func (s *Stager) StageOut(ctx context.Context, scratchDir string, entries []protocol.StageEntry) error {
	root, err := os.OpenRoot(scratchDir)
	if err != nil {
		return fmt.Errorf("staging: open scratch dir %q: %w", scratchDir, err)
	}
	defer root.Close()

	for i, e := range entries {
		if e.Direction != "OUT" && e.Direction != "INOUT" {
			continue
		}
		if err := s.stageOutEntry(ctx, root, scratchDir, i, e); err != nil {
			return fmt.Errorf("staging: copy-out %q: %w", e.Path, err)
		}
	}
	return nil
}

// stageOutEntry validates and transfers one OUT/INOUT entry. Extracted from
// StageOut both to keep it under the cyclop complexity limit and so the
// validated descriptor has a scope to be closed at.
//
// The built-in copy reads the descriptor openStageOutSource returned, never
// the path again — that is what closes the stage-out TOCTOU rather than
// merely narrowing it. An operator sync_command cannot be handed a
// descriptor, so the descriptor is closed first (holding a read handle while
// an external command opens the same path would contend for a Windows share
// mode to no purpose) and the command gets the absolute path string.
func (s *Stager) stageOutEntry(ctx context.Context, root *os.Root, scratchDir string, i int, e protocol.StageEntry) error {
	rel := filepath.Join(strconv.Itoa(i), filepath.Base(e.Path))
	f, err := openStageOutSource(root, rel)
	if err != nil {
		return err
	}
	defer f.Close()

	if !s.useBuiltin() {
		f.Close()
		return s.runSync(ctx, filepath.Join(scratchDir, rel), e.Path, e.ObjectType)
	}
	s.warnDefaults()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("fstat %q: %w", rel, err)
	}
	return copyFromFile(f, e.Path, info.Mode())
}

// Refusal classifications for [openStageOutSource]. Each names exactly ONE
// branch of it, so a test can assert the specific path it exercises instead of
// pattern-matching a message several branches happen to share.
//
// That distinction is load-bearing rather than tidiness. Two different things
// refuse a junction: errStageOutReparse, when the advisory Root.Lstat below
// saw a reparse point at rel's FINAL component, and errStageOutEscape, when
// the kernel refused a lookup that met one ANYWHERE in rel. Both messages
// necessarily say "junction", so a strings.Contains(err, "junction")
// assertion cannot tell them apart — the whole advisory block could be
// deleted with every test still green. errors.Is against these values can.
//
// Each sentinel's text IS the reason clause of the message the operator sees,
// so wrapping one costs no wording: the strings other tests match on
// ("symlink", "hardlink", "outside scratch dir") are unchanged.
var (
	errStageOutSymlink    = errors.New("is a symlink; sqi will not follow it to copy the file it points to")
	errStageOutReparse    = errors.New("is a reparse point (junction or symlink); sqi will not follow it to copy the file it points to")
	errStageOutEscape     = errors.New("resolves outside scratch dir, or reaches it through a symlink or junction")
	errStageOutUnreadable = errors.New("could not be opened")
	errStageOutNotRegular = errors.New("is not a regular file")
	errStageOutHardlink   = errors.New("has more than one hardlink; sqi will not copy a file that may alias content outside scratch")
)

// classifyStageOutOpenError turns a Root.OpenFile failure into an error that
// says which of three very different things happened. Reporting all of them as
// a containment breach — which this did until the classification was split out
// — sends an operator hunting an attack that never occurred:
//
//   - not-exist: the task simply produced no output. By far the most common
//     failure of the three and not an attack, so keep it boring.
//   - permission, sharing violation, I/O error: the daemon could not read a
//     source it is entirely entitled to — a scratch subdirectory chowned away
//     from it, a background child of the task still holding the file open
//     with a restrictive Windows share mode, a failing disk. Failing closed
//     is still correct and does not change; calling it an escape is not.
//   - anything else: the lookup was refused. os.Root fails a lookup that
//     leaves the root AND one that meets a reparse point, and both surface as
//     "path escapes from parent", so this is where a real attack lands.
func classifyStageOutOpenError(rel, rootName string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("open %q: %w", rel, err)
	case isAccessError(err):
		return fmt.Errorf("stage-out failed: %q %w from scratch dir %q; this is an access or I/O failure, not a containment breach: %w",
			rel, errStageOutUnreadable, rootName, err)
	default:
		return fmt.Errorf("stage-out refused: %q %w (scratch dir %q): %w", rel, errStageOutEscape, rootName, err)
	}
}

// openStageOutSource returns a validated, already-open read descriptor for one
// stage-out source, or an error naming why it was refused. rel is the source's
// path RELATIVE to root, which is rooted at the attempt's scratch directory.
//
// The elevated daemon always performs the transfer that follows, whether via
// the built-in copy or an operator's sync_command, so anything this misses is
// a daemon-level primitive under task control. A task owns its per-entry
// scratch subdirectory (StageIn's ChownRecursive hands it over) and can
// therefore replace any part of it.
//
// Opening THROUGH an os.Root is what makes this sound, and it replaces an
// earlier path-based check that was not:
//
//   - Containment is structural, not computed. The previous implementation
//     resolved src's parent with filepath.EvalSymlinks and compared it to
//     scratchDir. That is racy everywhere and, on Windows, simply wrong:
//     EvalSymlinks does not resolve a directory JUNCTION at all, so a task
//     that deleted its scratch subdirectory and ran `mklink /J` at an
//     arbitrary directory produced a path that passed containment, passed
//     the regular-file check (the file behind it is an ordinary file), and
//     was then followed by the copy. Creating a junction needs no privilege,
//     unlike an NTFS symlink, so this was reachable by any isolated task —
//     one-shot, no race required.
//   - Reparse points are refused by the kernel. os.Root maps to
//     OBJ_DONT_REPARSE on Windows (any reparse point anywhere in the relative
//     path fails the lookup) and to openat with per-component O_NOFOLLOW on
//     POSIX. There is no window between deciding and opening, because they
//     are the same operation.
//   - There is no ".." to normalize away, so no escape by construction.
//
// Deliberate platform asymmetry, stated here rather than discovered later:
// os.Root refuses EVERY reparse point on Windows, including one that points
// back inside the root, while on POSIX it FOLLOWS a symlink that stays within
// the root. That difference is harmless for this caller — nothing legitimate
// under scratch is ever a link (prepareEntries creates only directories, and
// copyTree explicitly skips non-regular entries) — and the advisory Lstat
// below means POSIX keeps refusing any symlink at rel outright, preserving
// the pre-H3 behavior and wording.
//
// On POSIX this is therefore NOT a pure strengthening, and should not be read
// as one. Stage-out used to reach copyFile, whose O_NOFOLLOW refused ANY
// symlink at the source's final component authoritatively, at the open. Now
// Root.OpenFile FOLLOWS a symlink that stays inside the root, so the only
// thing refusing an in-root symlink is the racy advisory Lstat above it. That
// narrowing is accepted deliberately: it is not an escalation, because
// everything under the per-attempt scratch directory is already task-owned
// and task-writable (StageIn's ChownRecursive hands it over), so a task gains
// nothing by symlinking one scratch path at another that it could not get by
// writing the same bytes directly. Meanwhile the property that actually
// matters — no escape from scratch — moved from a racy, and on Windows
// simply wrong, filepath.EvalSymlinks computation to a kernel-enforced
// lookup. A weaker check on something the task already controls, in exchange
// for a sound one on the boundary.
//
// The remaining two checks run on the DESCRIPTOR, never on the path again:
//
//   - regular-file-only: refuses device nodes, FIFOs and sockets. None is a
//     legitimate task output. (This is also what refuses a DIRECTORY-typed
//     stage-out entry, which sqi has never supported.)
//   - link count > 1 refused: a hardlink IS a regular file — it shares one
//     inode with whatever it is linked to — so it passes every check above
//     yet leaks its link partner identically to a symlink once copied.
//
// The caller closes the returned descriptor. For the built-in copy it is
// handed straight to copyFromFile, so the inode validated here is the inode
// read — there is no second lookup for a task to race. An operator-configured
// sync_command receives path STRINGS and no descriptor, so its residual race
// is structurally unclosable and stays documented in
// docs/worker-configuration.md; what this function DOES give that mechanism
// is refusal of the one-shot swap, which previously succeeded on Windows.
func openStageOutSource(root *os.Root, rel string) (*os.File, error) {
	// ADVISORY ONLY, and deliberately so. Root.Lstat does not follow the
	// final component, so this turns the two common attacks into a precise
	// diagnostic instead of the bare "path escapes from parent" the open
	// would otherwise produce. It is racy, and that costs nothing: the open
	// below is the security boundary and is safe whatever this saw.
	if li, err := root.Lstat(rel); err == nil {
		switch {
		case li.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("stage-out refused: %q %w", rel, errStageOutSymlink)
		case li.Mode()&os.ModeIrregular != 0:
			return nil, fmt.Errorf("stage-out refused: %q %w", rel, errStageOutReparse)
		}
	}

	f, err := root.OpenFile(rel, os.O_RDONLY, 0)
	if err != nil {
		return nil, classifyStageOutOpenError(rel, root.Name(), err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("fstat %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("stage-out refused: %q %w (mode %s)", rel, errStageOutNotRegular, info.Mode())
	}
	linked, err := hasExtraHardlinks(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("fstat %q: %w", rel, err)
	}
	if linked {
		f.Close()
		return nil, fmt.Errorf("stage-out refused: %q %w", rel, errStageOutHardlink)
	}
	return f, nil
}

// Cleanup removes the scratch directory, logging (not returning) any error.
func (s *Stager) Cleanup(scratchDir string) {
	if scratchDir == "" {
		return
	}
	if err := os.RemoveAll(scratchDir); err != nil {
		s.logger.WarnContext(context.Background(), "staging: cleanup failed", slog.String("scratch_dir", scratchDir), slog.Any("error", err))
	}
}

// transfer moves bytes for one staged path, via the built-in copy or the
// shell sync command. It logs a one-time warning when defaults are in effect.
func (s *Stager) transfer(ctx context.Context, src, dest, objectType string) error {
	if s.useBuiltin() {
		s.warnDefaults()
		return builtinCopy(ctx, src, dest)
	}
	return s.runSync(ctx, src, dest, objectType)
}

// warnDefaults logs a one-time warning when staging fell back to defaults
// (not an explicit scratch dir or `builtin` sentinel).
func (s *Stager) warnDefaults() {
	if s.scratchBase != "" || s.syncCommand == builtinSentinel {
		return
	}
	s.warnOnce.Do(func() {
		s.logger.WarnContext(context.Background(),
			"staging not configured — using default scratch and built-in copy; set staging.scratch_dir/staging.sync_command for production",
			slog.String("scratch_dir", s.effectiveScratch()))
	})
}

// runSync renders the sync command template and executes it. The template is
// split on whitespace; {src}, {dest}, {object_type} placeholders are replaced in
// each field. stderr is captured and surfaced on failure.
func (s *Stager) runSync(ctx context.Context, src, dest, objectType string) error {
	fields := strings.Fields(s.syncCommand)
	if len(fields) == 0 {
		return errors.New("empty sync command")
	}
	rep := strings.NewReplacer("{src}", src, "{dest}", dest, "{object_type}", objectType)
	for i := range fields {
		fields[i] = rep.Replace(fields[i])
	}
	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...) //nolint:gosec // operator-configured command
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// builtinCopy copies src to dest without an external command — the default /
// `builtin` transfer used when no shell sync_command is configured. It never
// follows a symlink at src: os.Lstat, not os.Stat, decides the mode (a
// declared ObjectType cannot override it) — a real directory is copied as a
// whole tree, a real regular file as a single file, anything else (symlink,
// device node, FIFO, socket) is refused outright.
//
// Since H3 this is the STAGE-IN path only. Stage-out no longer routes through
// here at all: stageOutEntry hands openStageOutSource's validated descriptor
// straight to copyFromFile, so there is no path-based mode decision left for
// a task to race. Destination parents are created and the source file mode is
// preserved (ownership and xattrs are not — adequate for worker-local
// scratch).
func builtinCopy(ctx context.Context, src, dest string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("lstat %q: %w", src, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("copy refused: %q is a symlink; sqi will not follow it", src)
	case info.IsDir():
		return copyTree(ctx, src, dest)
	case info.Mode().IsRegular():
		return copyFile(src, dest, info.Mode())
	default:
		return fmt.Errorf("copy refused: %q is not a regular file or directory (mode %s)", src, info.Mode())
	}
}

// copyFile opens src by path and copies it to dest, refusing to follow a
// symlink at either end.
//
// Since H3 this is the STAGE-IN path (and copyTree's recursion) only —
// stage-out goes through openStageOutSource + copyFromFile, which never
// re-opens by path. A stage-in source is a job-declared asset outside any
// scratch directory, so there is no os.Root to open it through and
// O_NOFOLLOW remains the right tool: a task that swapped its declared output
// for a symlink between an upstream check and this open must not have the
// daemon read whatever it points to. O_NOFOLLOW does NOT refuse a hardlink,
// though — a hardlink IS a regular file, so it opens successfully — which is
// why in.Stat()'s link count below runs on the OPENED DESCRIPTOR, pinning the
// inode this call actually reads.
func copyFile(src, dest string, mode os.FileMode) error {
	in, err := os.OpenFile(src, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer in.Close()
	fdInfo, err := in.Stat()
	if err != nil {
		return fmt.Errorf("fstat %q: %w", src, err)
	}
	if !fdInfo.Mode().IsRegular() {
		return fmt.Errorf("copy refused: opened %q is not a regular file (mode %s)", src, fdInfo.Mode())
	}
	if linked, err := hasExtraHardlinks(in); err != nil {
		return fmt.Errorf("fstat %q: %w", src, err)
	} else if linked {
		return fmt.Errorf("copy refused: opened %q has more than one hardlink; sqi will not copy a file that may alias content outside scratch", src)
	}
	return copyFromFile(in, dest, mode)
}

// copyFromFile writes an already-open, already-validated source descriptor to
// dest. Splitting this out of copyFile is what lets stage-out read the exact
// descriptor openStageOutSource validated, leaving no second path lookup for
// a task to race.
//
// dest is written via the same remove-then-O_EXCL|O_NOFOLLOW pattern as
// [isolation.WriteFileFchown] (see its doc for the full reasoning): any
// existing entry at dest is unlinked (never followed) before a fresh file is
// created, so a task-planted symlink at dest is removed, not written through,
// and a hardlink there loses a link rather than having its target inode
// truncated and overwritten with task-controlled bytes. On Windows the
// O_EXCL create carries FILE_FLAG_OPEN_REPARSE_POINT via Go's syscall.Open,
// so a reparse point at dest is refused there too despite noFollowFlag being
// a no-op. A legitimate re-run overwriting a prior real output still succeeds
// (the old inode is simply replaced by a new one carrying src's mode) — only
// an attacker-swap or a lost race against a concurrent writer (EEXIST,
// failing closed) behaves differently.
func copyFromFile(in *os.File, dest string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(dest), err)
	}
	if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove existing %q: %w", dest, err)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, mode.Perm())
	if err != nil {
		return fmt.Errorf("create %q: %w", dest, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %q -> %q: %w", in.Name(), dest, err)
	}
	return out.Close()
}

func copyTree(ctx context.Context, src, dest string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks/special files in local scratch
		}
		return copyFile(path, target, info.Mode())
	})
}
