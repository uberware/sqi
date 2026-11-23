// SPDX-License-Identifier: AGPL-3.0-or-later

// Package staging implements the stage_locally path delivery: copying job
// inputs to worker-local scratch before a task runs and outputs back afterward.
// Each path is copied by an operator-configured sync command or, without one,
// by the built-in copy. The scratch destination paths are returned as path-map
// rules so the other deliveries advertise them.
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

// Stager copies staged paths with an operator-configured sync command, or with
// the built-in copy when sync_command is empty or `builtin`.
type Stager struct {
	scratchBase string
	syncCommand string
	defaults    bool
	logger      *slog.Logger
	warnOnce    sync.Once

	// ancestorMode is the mode StageIn creates the shared scratch base and
	// job directory at: 0o711 when [WithIsolationCapable] reports the worker
	// can isolate, 0o750 otherwise. It is fixed for the Stager's lifetime;
	// see WithIsolationCapable.
	ancestorMode os.FileMode
}

// Option configures a [Stager] at construction. See [WithIsolationCapable].
type Option func(*Stager)

// WithIsolationCapable sets, once at worker boot, whether this worker can run
// tasks as another user at all. The caller passes the same root check
// cmd/sqi-worker's effectiveSessionRoot uses. It is a per-worker setting, not
// "did this assignment carry a credential": scratchBase and scratchBase/jobID
// are shared by every attempt of every job, including isolated and
// non-isolated queues on one worker, so deciding per assignment would let
// whichever task first creates the base fix its mode for every later attempt.
// A non-isolated task creating the base at 0750 would make every later
// isolated task fail ValidateTraversable against a base it cannot widen. A
// capable worker creates both directories at 0711; any other keeps 0750,
// which is also the mode when this option is not passed.
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
// run-as-user isolation creates its shared scratch ancestors traversable;
// without it they are created 0750.
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
// isolation.ValidateTraversable) validates the path StageIn uses instead of
// duplicating this default.
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
// scratch directory. IN/INOUT inputs are copied into scratch; OUT entries only
// get their scratch destination created (the task writes the output there,
// and [Stager.StageOut] copies it back afterward). The rule for an OUT entry
// lets the other deliveries redirect the task's OUTPUT paths into scratch;
// without it the task writes to the real path and copy-out fails on a missing
// scratch file. On any failure the partial scratch directory is removed.
//
// cred is the run-as-user credential for the task that will read and write
// these paths, or nil when the assignment carries no isolation. Staging
// always runs as the worker (the sync command is operator-configured, not job
// code; internal/worker/session and internal/worker/executor are the launch
// sites that carry a credential), so everything it writes is owned by the
// worker. When cred is non-nil, StageIn hands the per-attempt scratch
// directory to cred (isolation.ChownRecursive) so the isolated task can read
// its staged inputs and write into its staged output directories.
//
// The scratch base (s.effectiveScratch()) and scratchBase/jobID are shared by
// every attempt of every job and every run-as-user identity, so their mode
// (s.ancestorMode) is fixed at construction (see [WithIsolationCapable]) and
// they are never chowned: chowning them to one identity would give it nothing
// (search permission does not need ownership) and break every other
// identity's access. os.MkdirAll does not change the mode of a directory that
// already exists, so a pre-existing scratch_dir with a narrower mode is
// reported by isolation.ValidateTraversable (at boot when isolation.required
// is set, otherwise per assignment below) instead of being widened. Both
// ancestors must be traversable because Go's forkAndExecInChild sets the
// process credentials before chdir; otherwise the isolated task cannot reach
// its staged inputs even though scratchDir itself is chowned to it.
func (s *Stager) StageIn(ctx context.Context, jobID, attemptID string, entries []protocol.StageEntry, cred *isolation.Credential) ([]protocol.PathMapRule, string, error) {
	base := s.effectiveScratch()
	jobDir := filepath.Join(base, jobID)
	scratchDir := filepath.Join(jobDir, attemptID)

	// Validate the ancestors whenever this attempt runs as another user.
	// cmd/sqi-worker's boot-time validateIsolationAncestors runs only when
	// isolation.required is set, so other isolated attempts get the same check
	// and message here, against the base this call creates, failing only this
	// task rather than the worker.
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
// outputs into scratch. Input bytes (IN/INOUT) are copied in by transfer; OUT
// entries are produced by the task, so only their scratch directory is
// created. The per-index layout (so same-basename entries never collide)
// matches what StageOut uses for copy-back.
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
// Every source is opened through one os.Root at scratchDir, so the kernel
// enforces containment and refuses reparse points at open time (see
// openStageOutSource). The check runs before both the built-in copy and an
// operator-configured sync_command, the one place it can cover both: whether
// a sync_command dereferences symlinks depends on that command ("rsync -a"
// preserves them; "rsync -aL" or plain "cp" follow them).
//
// os.OpenRoot(scratchDir) is itself a lookup by path, and on Windows it
// follows a reparse point at scratchDir. Ownership protects it instead:
// replacing scratchDir means creating an entry in its parent, the per-job
// directory StageIn creates as the worker, which is never chowned (POSIX) or
// ACL'd (Windows) to the task; only the per-attempt leaf below it is handed
// over by ChownRecursive. A task therefore cannot substitute the root every
// lookup in this loop is anchored to.
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

// stageOutEntry validates and transfers one OUT/INOUT entry, giving the
// validated descriptor a scope to be closed in.
//
// The built-in copy reads the descriptor openStageOutSource returned and never
// the path again, which leaves no window between check and use. An operator
// sync_command cannot be handed a descriptor, so the descriptor is closed
// first (a read handle held while an external command opens the same path
// would contend for a Windows share mode) and the command gets the absolute
// path.
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

// Refusal classifications for [openStageOutSource]. Each names one branch of
// it, so a test can assert the branch it exercises instead of matching a
// message several branches share.
//
// Two of them refuse a junction: errStageOutReparse, when the advisory
// Root.Lstat saw a reparse point at rel's final component, and
// errStageOutEscape, when the kernel refused a lookup that met one anywhere in
// rel. Both messages say "junction", so a strings.Contains(err, "junction")
// assertion cannot tell them apart; errors.Is against these values can.
//
// Each sentinel's text is the reason clause of the message the operator sees.
// Other tests match on the substrings "symlink", "hardlink" and "outside
// scratch dir".
var (
	errStageOutSymlink    = errors.New("is a symlink; sqi will not follow it to copy the file it points to")
	errStageOutReparse    = errors.New("is a reparse point (junction or symlink); sqi will not follow it to copy the file it points to")
	errStageOutEscape     = errors.New("resolves outside scratch dir, or reaches it through a symlink or junction")
	errStageOutUnreadable = errors.New("could not be opened")
	errStageOutNotRegular = errors.New("is not a regular file")
	// Says "beyond scratch", not "outside scratch": that substring belongs to
	// errStageOutEscape alone, and tests (including
	// test/integration/isolation_windows_test.go) match on it to identify the
	// containment refusal. A hardlink refusal must not satisfy an assertion
	// written about an escape.
	errStageOutHardlink = errors.New("has more than one hardlink; sqi will not copy a file that may alias content beyond scratch")
)

// classifyStageOutOpenError turns a Root.OpenFile failure into an error that
// says which of three things happened, so an operator is not sent looking for
// an attack that did not occur:
//
//   - not-exist: the task produced no output. The most common of the three,
//     and not an attack.
//   - permission, sharing violation, I/O error: the worker could not read a
//     source it is entitled to read (a scratch subdirectory chowned away from
//     it, a background child of the task holding the file open with a
//     restrictive Windows share mode, a failing disk). The stage-out still
//     fails, but is not reported as an escape.
//   - anything else: the lookup was refused. os.Root fails a lookup that
//     leaves the root and one that meets a reparse point, and both surface as
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

// openStageOutSource returns a validated, open read descriptor for one
// stage-out source, or an error naming why it was refused. rel is the source's
// path relative to root, which is the attempt's scratch directory.
//
// The worker, which may be elevated, performs the transfer that follows
// through the built-in copy or an operator's sync_command, so anything this
// misses hands task code a worker-level primitive. A task owns its per-entry
// scratch subdirectory (StageIn's ChownRecursive hands it over) and can
// replace any part of it.
//
// Opening through an os.Root gives:
//
//   - Structural containment. A path check with filepath.EvalSymlinks would
//     be racy everywhere and, on Windows, wrong: EvalSymlinks does not
//     resolve a directory junction, so a task could replace its scratch
//     subdirectory with `mklink /J` to any directory and have the ordinary
//     file behind it copied. Creating a junction needs no privilege.
//   - An atomic lookup: deciding and opening are one operation. The kernel
//     resolves every component of rel as part of the open: openat with
//     per-component O_NOFOLLOW on POSIX, an OBJ_DONT_REPARSE handle walk on
//     Windows. The two refuse different things; see below.
//   - No ".." to normalize away.
//
// Windows os.Root passes OBJ_DONT_REPARSE, so the kernel fails a lookup that
// meets a reparse point anywhere in rel. Go then reads the link target and
// retries it as a path inside the root (os.readReparseLinkAt, then doInRoot's
// errSymlink case, GOROOT/src/os/root_windows.go and root_openat.go), so what
// refuses a junction is the retry. A junction's stored target is an
// IO_REPARSE_TAG_MOUNT_POINT substitute name, which is always absolute (Go's
// readReparseLinkHandle has no relative branch for that tag, unlike
// IO_REPARSE_TAG_SYMLINK's SYMLINK_FLAG_RELATIVE), and splitPathInRoot ->
// rootCleanPath -> filepathlite.IsLocal rejects a volume-qualified path as
// errPathEscapes. Every junction is therefore refused, including one pointing
// back inside the root.
//
// A relative NTFS directory symlink whose target stays inside the root is
// followed, as POSIX follows an in-root symlink. An isolated task cannot
// create one (any NTFS symlink needs SeCreateSymbolicLinkPrivilege or
// Developer Mode), and the retried path stays inside the root, so it cannot
// leave scratch. Nothing legitimate under scratch is a link (prepareEntries
// creates only directories, and copyTree skips non-regular entries), and the
// advisory Lstat refuses a symlink (POSIX) or any reparse point (Windows) at
// rel's final component.
//
// On POSIX this is weaker than copyFile's O_NOFOLLOW, which refuses any
// symlink at the source's final component at the open: Root.OpenFile follows
// a symlink that stays inside the root, so only the racy advisory Lstat
// refuses an in-root symlink. That gives a task nothing: everything under the
// per-attempt scratch directory is already task-owned and task-writable, so a
// symlink from one scratch path to another yields nothing the task could not
// write directly. Escape from scratch is refused by the kernel lookup.
//
// Two more checks run on the descriptor, never on the path:
//
//   - regular files only: refuses device nodes, FIFOs and sockets, none of
//     which is a legitimate task output, and a directory-typed stage-out
//     entry, which sqi does not support.
//   - link count > 1 refused: a hardlink is a regular file sharing an inode
//     with whatever it is linked to, so it passes every check above yet
//     exposes its link partner once copied.
//
// The caller closes the returned descriptor. The built-in copy hands it to
// copyFromFile, so the inode validated here is the inode read. An
// operator-configured sync_command receives path strings and no descriptor,
// so its remaining race cannot be closed here and is documented in
// docs/worker-configuration.md; this function still refuses the one-shot swap
// that a path-based check lets through on Windows.
func openStageOutSource(root *os.Root, rel string) (*os.File, error) {
	// Advisory only. Root.Lstat does not follow the final component, so this
	// gives the two common attacks a precise message instead of the bare
	// "path escapes from parent" the open would produce. It is racy, which
	// does not matter: the open below is the security boundary.
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

// builtinCopy copies src to dest without an external command: the transfer
// used when sync_command is empty or `builtin`. It never follows a symlink at
// src: os.Lstat, not os.Stat, decides the mode (a declared ObjectType cannot
// override it). A directory is copied as a whole tree, a regular file as a
// single file, and anything else (symlink, device node, FIFO, socket) is
// refused.
//
// It is used for stage-in only; stageOutEntry hands openStageOutSource's
// validated descriptor to copyFromFile, so stage-out makes no path-based mode
// decision for a task to race. Destination parents are created and the source
// file mode is preserved; ownership and xattrs are not.
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
// It is used for stage-in (and copyTree's recursion) only; stage-out goes
// through openStageOutSource and copyFromFile, which never reopen by path. A
// stage-in source is a job-declared asset outside any scratch directory, so
// there is no os.Root to open it through and O_NOFOLLOW is used instead: a
// symlink swapped in between an upstream check and this open must not have
// the worker read whatever it points to. O_NOFOLLOW does not refuse a
// hardlink, which is a regular file, so the link count is checked on the
// opened descriptor, the inode this call reads.
//
// The link-count refusal applies on both platforms, so a job input asset with
// a second NTFS hardlink is refused at stage-in on Windows too, as are inputs
// delivered by content-addressed or dedup asset stores and
// "rsync --link-dest"-style delivery. A hardlink is the file itself: chowning
// the scratch copy to the run-as-user identity would chown the original too
// (see docs/worker-configuration.md). This refuses some legitimate work;
// TestCopyFile_RefusesHardlinkedStageInSourceOnWindows pins it.
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
		return fmt.Errorf("copy refused: opened %q has more than one hardlink; sqi will not copy a file that may alias content beyond scratch", src)
	}
	return copyFromFile(in, dest, mode)
}

// copyFromFile writes an already-open, already-validated source descriptor to
// dest. Stage-out uses it to read the descriptor openStageOutSource
// validated, with no second path lookup for a task to race.
//
// dest is written with the same remove-then-O_EXCL|O_NOFOLLOW pattern as
// [isolation.WriteFileFchown]: any existing entry at dest is unlinked (never
// followed) before a fresh file is created, so a task-planted symlink at dest
// is removed, not written through, and a hardlink there loses a link rather
// than having its target inode truncated and overwritten with task-controlled
// bytes. On Windows the O_EXCL create carries FILE_FLAG_OPEN_REPARSE_POINT
// via Go's syscall.Open, so a reparse point at dest is refused there too
// although noFollowFlag is a no-op. A re-run overwriting a prior real output
// succeeds (the old inode is replaced by a new one carrying src's mode); a
// lost race against a concurrent writer fails with EEXIST.
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
