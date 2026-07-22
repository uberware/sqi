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
// identity — so they are created traversable FROM BIRTH (0711) rather than
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
// here. Only the per-attempt leaf (scratchDir itself, via ChownRecursive
// below) is chowned, since it belongs to exactly this one attempt/identity.
// Go's forkAndExecInChild sets process credentials BEFORE chdir, so without
// both ancestors being traversable the isolated task cannot reach its own
// staged inputs even though scratchDir itself is correctly chowned.
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

	if err := os.MkdirAll(base, 0o711); err != nil { //nolint:gosec // G301: 0711 (search-only for group/other) is the intentional traversable-from-birth mode; see the doc comment above
		return nil, "", fmt.Errorf("staging: create scratch base %q: %w", base, err)
	}
	if err := os.MkdirAll(jobDir, 0o711); err != nil { //nolint:gosec // G301: same as above
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
func (s *Stager) StageOut(ctx context.Context, scratchDir string, entries []protocol.StageEntry) error {
	for i, e := range entries {
		if e.Direction != "OUT" && e.Direction != "INOUT" {
			continue
		}
		src := filepath.Join(scratchDir, strconv.Itoa(i), filepath.Base(e.Path))
		if err := s.transfer(ctx, src, e.Path, e.ObjectType); err != nil {
			return fmt.Errorf("staging: copy-out %q: %w", e.Path, err)
		}
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
// `builtin` transfer used when no shell sync_command is configured. What src
// actually is on disk decides the mode (a declared ObjectType cannot override
// it): a directory is copied as a whole tree, anything else as a single file.
// Destination parents are created and the source file mode is preserved
// (ownership and xattrs are not — adequate for worker-local scratch).
func builtinCopy(ctx context.Context, src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %q: %w", src, err)
	}
	if info.IsDir() {
		return copyTree(ctx, src, dest)
	}
	return copyFile(src, dest, info.Mode())
}

func copyFile(src, dest string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(dest), err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
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
