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
}

// New returns a Stager. scratchBase/syncCommand come from worker config;
// defaults enables the built-in copy + TEMP scratch when staging is
// otherwise unconfigured.
func New(scratchBase, syncCommand string, defaults bool, logger *slog.Logger) *Stager {
	return &Stager{scratchBase: scratchBase, syncCommand: syncCommand, defaults: defaults, logger: logger}
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
// identity — so, ONLY when this call actually carries an isolated identity
// (cred != nil), they are created traversable FROM BIRTH (0711) rather than
// created narrow and widened after the fact: chowning either to any one
// identity would grant that identity nothing extra (search doesn't require
// ownership) while breaking every other attempt's/user's access, and
// mutating an EXISTING directory's mode is exactly the anti-pattern this
// codebase's isolation split eliminates elsewhere (see
// workerconfig.LoadOrCreateWorkerID's doc). Creating fresh at 0711 is a
// different, safe operation: os.MkdirAll never touches the mode of a
// directory that already exists, so a pre-existing scratch_dir at a
// narrower mode is caught by cmd/sqi-worker's boot-time
// isolation.ValidateTraversable check instead of being silently widened
// here. When cred is nil, base/jobDir are created at the narrower 0750
// instead — the same widening this call would otherwise perform
// unconditionally on every worker, isolated or not, which is exactly the
// anti-pattern this codebase's other unconditional-widening fix (the session
// root, see workerconfig.LoadOrCreateWorkerID's doc) already reverted once;
// that reasoning had not yet reached this file. A worker that never engages
// isolation on this scratch base keeps the pre-isolation 0750 mode instead of
// gaining a needless traversable-by-anyone directory. Only the per-attempt
// leaf (scratchDir itself, via ChownRecursive below) is chowned, since it
// belongs to exactly this one attempt/identity. Go's forkAndExecInChild sets
// process credentials BEFORE chdir, so without both ancestors being
// traversable the isolated task cannot reach its own staged inputs even
// though scratchDir itself is correctly chowned.
func (s *Stager) StageIn(ctx context.Context, jobID, attemptID string, entries []protocol.StageEntry, cred *isolation.Credential) ([]protocol.PathMapRule, string, error) {
	base := s.effectiveScratch()
	jobDir := filepath.Join(base, jobID)
	scratchDir := filepath.Join(jobDir, attemptID)
	ancestorMode := os.FileMode(0o750)
	if cred != nil {
		ancestorMode = 0o711
	}

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

	if err := os.MkdirAll(base, ancestorMode); err != nil {
		return nil, "", fmt.Errorf("staging: create scratch base %q: %w", base, err)
	}
	if err := os.MkdirAll(jobDir, ancestorMode); err != nil {
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
// subdirectory (<scratchDir>/<i>/<basename>) matches what copyInEntries created.
//
// validateStageOutSource runs BEFORE transfer() for every entry — upstream of
// BOTH the built-in copy and an operator-configured sync_command, which is the
// only place a check can cover both identically. sqi cannot audit an
// arbitrary sync_command template: whether it dereferences symlinks is a
// property of that command ("rsync -a" preserves them; "rsync -aL" or plain
// "cp" follow them). See the function's own doc for the full threat model.
func (s *Stager) StageOut(ctx context.Context, scratchDir string, entries []protocol.StageEntry) error {
	for i, e := range entries {
		if e.Direction != "OUT" && e.Direction != "INOUT" {
			continue
		}
		src := filepath.Join(scratchDir, strconv.Itoa(i), filepath.Base(e.Path))
		if err := validateStageOutSource(scratchDir, src); err != nil {
			return fmt.Errorf("staging: copy-out %q: %w", e.Path, err)
		}
		if err := s.transfer(ctx, src, e.Path, e.ObjectType); err != nil {
			return fmt.Errorf("staging: copy-out %q: %w", e.Path, err)
		}
	}
	return nil
}

// validateStageOutSource refuses to copy a stage-out source that is not an
// ordinary, scratch-contained regular file — the boundary check for a task
// that plants something other than its declared output at the deterministic
// scratch path StageOut reads from. Root (the daemon) always performs this
// transfer, whether via the built-in copy or an operator's sync_command, so
// anything this check misses is a root-level primitive under task control:
//
//   - os.Lstat, never Stat/os.Stat: a Stat-based check FOLLOWS a final
//     symlink. A task that replaces its declared output path with a symlink
//     to, say, /etc/shadow would have root read THAT file's bytes and copy
//     them to e.Path — arbitrary file disclosure as root, gated only by
//     whatever e.Path happens to be readable by afterward.
//   - regular-file-only: refuses symlinks (caught above already, but kept as
//     an explicit, self-documenting branch) as well as device nodes, FIFOs,
//     and sockets — none of these is a legitimate task output.
//   - link count > 1 refused: a hardlink planted inside scratch passes the
//     regular-file check above (a hardlink IS a regular file — it shares one
//     inode with whatever it is linked to) yet leaks its link partner
//     identically to a symlink once copied. fs.protected_hardlinks (Linux)
//     narrows creating a hardlink to a file the creator cannot already read,
//     but it is a HOST kernel setting sqi does not control and must not
//     assume is enabled.
//   - scratch containment: src's REAL (symlink-resolved) parent directory
//     must sit inside scratchDir. A task that deletes one of the per-entry
//     scratch subdirectories it owns (chowned by StageIn's ChownRecursive)
//     and replaces it with a symlink to an arbitrary directory would
//     otherwise let a same-named regular file elsewhere satisfy every check
//     above while sitting entirely outside scratch. Only the parent
//     directory is resolved via filepath.EvalSymlinks — the final path
//     component itself must NOT be resolved, since that is exactly the
//     dereference the Lstat check above exists to refuse.
//
// This check runs against a PATH, not a descriptor, so by itself it only
// closes a one-shot swap — a task that plants the bad entry once, before
// this Lstat ever runs. It does NOT by itself close a race: nothing kills a
// task's process group on a SUCCESSFUL exit (see executor.runTask /
// killAndWait), so a still-running background child that still owns a
// scratch subdirectory can swap a hardlink in AFTER this check passes and
// BEFORE the transfer actually reads the file.
//
//   - For the built-in copy, that race IS closed: copyFile re-validates
//     regular-file-ness and link count on the DESCRIPTOR it opened
//     (in.Stat(), not another path-based stat), which pins the inode
//     actually read — see copyFile's own doc. This check and that one
//     together close both the one-shot and the race variant for
//     builtinCopy/copyFile, which is the mechanism sqi actually controls.
//   - For an operator-configured sync_command, the race is NOT closed and
//     cannot be: sqi hands the command path STRINGS ({src}/{dest}), never
//     descriptors, so there is no fd for sqi to pin between this check and
//     whatever the command does with those paths — the TOCTOU is
//     structurally unclosable for that mechanism. Whether the command
//     itself follows a symlink at either end (dest, e.Path, is the real,
//     operator/job-known destination; src is the scratch path this check
//     just validated) is entirely a property of the command sqi cannot
//     inspect ("rsync -a" doesn't; "rsync -aL" or plain "cp" do). An
//     operator-configured sync_command therefore remains the operator's
//     responsibility on both counts — it MUST NOT dereference symlinks, and
//     its residual TOCTOU exposure is inherent to handing it paths at all —
//     see docs/worker-configuration.md's sync_command warning for the full,
//     honest statement of what is and is not mitigated.
func validateStageOutSource(scratchDir, src string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("lstat %q: %w", src, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("stage-out refused: %q is a symlink; sqi will not follow it to copy the file it points to", src)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("stage-out refused: %q is not a regular file (mode %s)", src, info.Mode())
	}
	if linked, err := hasExtraHardlinks(info); err != nil {
		return fmt.Errorf("stat %q: %w", src, err)
	} else if linked {
		return fmt.Errorf("stage-out refused: %q has more than one hardlink; sqi will not copy a file that may alias content outside scratch", src)
	}

	absScratch, err := filepath.Abs(scratchDir)
	if err != nil {
		return fmt.Errorf("resolve scratch dir %q: %w", scratchDir, err)
	}
	resolvedScratch, err := filepath.EvalSymlinks(absScratch)
	if err != nil {
		return fmt.Errorf("resolve scratch dir %q: %w", scratchDir, err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(src))
	if err != nil {
		return fmt.Errorf("resolve %q: %w", filepath.Dir(src), err)
	}
	rel, err := filepath.Rel(resolvedScratch, resolvedParent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("stage-out refused: %q resolves outside scratch dir %q", src, scratchDir)
	}
	return nil
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
// `builtin` transfer used when no shell sync_command is configured. Used for
// both directions (stage-in and stage-out), so it never follows a symlink at
// src: os.Lstat, not os.Stat, decides the mode (a declared ObjectType cannot
// override it) — a real directory is copied as a whole tree, a real regular
// file as a single file, anything else (symlink, device node, FIFO, socket)
// is refused outright. For stage-out specifically this is defense in depth
// behind [validateStageOutSource]'s upstream boundary check — that check
// covers an operator's sync_command too, which this function is never
// involved in, but keeping builtinCopy itself symlink-safe closes the same
// TOCTOU window the boundary check leaves between its own Lstat and this
// call. Destination parents are created and the source file mode is
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

// copyFile copies src to dest, refusing to follow a symlink at either end.
//
// src is opened with O_NOFOLLOW (in addition to builtinCopy's own Lstat
// check, closing the TOCTOU between that check and this open): a task that
// swaps its declared output for a symlink between the two must not have root
// read whatever it points to. O_NOFOLLOW does NOT refuse a hardlink, though —
// a hardlink IS a regular file, so it opens successfully — which matters for
// the stage-out path specifically: validateStageOutSource's Nlink check runs
// against the PATH before this call, and nothing kills a task's process
// group on a SUCCESSFUL exit (see executor.runTask/killAndWait), so a
// still-running background child owning the scratch entry can swap a
// hardlink in between that path-based check and this open. in.Stat() below
// re-validates on the OPENED DESCRIPTOR, which pins the inode this call
// actually reads — a swap after this point cannot change what fd `in` names,
// closing that window. This re-check is unconditional (stage-in has no
// upstream path-based guard to race, but re-validating costs nothing and
// keeps this function's safety self-contained rather than dependent on which
// caller ran a check first).
//
// dest is written via the same remove-then-O_EXCL|O_NOFOLLOW pattern as
// [isolation.WriteFileFchown] (see its doc for the full reasoning): any
// existing entry at dest is unlinked (never followed) before a fresh file is
// created, so a task-planted symlink at dest is removed, not written
// through, and a hardlink there loses a link rather than having its target
// inode truncated and overwritten with task-controlled bytes. A legitimate
// re-run overwriting a prior real output still succeeds (the old inode is
// simply replaced by a new one carrying src's mode) — only an attacker-swap
// or a lost race against a concurrent writer (EEXIST, failing closed) behaves
// differently from the previous O_TRUNC-based overwrite.
func copyFile(src, dest string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(dest), err)
	}
	in, err := os.OpenFile(src, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer in.Close()
	// Re-validate on the descriptor we actually opened, not the path we
	// opened it from — see this function's doc for why a path-based check
	// upstream (validateStageOutSource) is not enough on its own.
	fdInfo, err := in.Stat()
	if err != nil {
		return fmt.Errorf("fstat %q: %w", src, err)
	}
	if !fdInfo.Mode().IsRegular() {
		return fmt.Errorf("copy refused: opened %q is not a regular file (mode %s)", src, fdInfo.Mode())
	}
	if linked, err := hasExtraHardlinks(fdInfo); err != nil {
		return fmt.Errorf("fstat %q: %w", src, err)
	} else if linked {
		return fmt.Errorf("copy refused: opened %q has more than one hardlink; sqi will not copy a file that may alias content outside scratch", src)
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
		return fmt.Errorf("copy %q -> %q: %w", src, dest, err)
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
