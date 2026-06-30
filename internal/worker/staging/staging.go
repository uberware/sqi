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
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// Stager copies staged paths via an external sync command.
type Stager struct {
	scratchBase string
	syncCommand string
	logger      *slog.Logger
}

// New returns a Stager. scratchBase and syncCommand come from worker config.
func New(scratchBase, syncCommand string, logger *slog.Logger) *Stager {
	return &Stager{scratchBase: scratchBase, syncCommand: syncCommand, logger: logger}
}

// Configured reports whether both scratch dir and sync command are set.
func (s *Stager) Configured() bool {
	return s.scratchBase != "" && s.syncCommand != ""
}

// StageIn copies every IN/INOUT entry into a per-attempt scratch directory and
// returns one path-map rule per entry (original path -> scratch path) plus the
// scratch directory. On any failure the partial scratch directory is removed.
func (s *Stager) StageIn(ctx context.Context, jobID, attemptID string, entries []protocol.StageEntry) ([]protocol.PathMapRule, string, error) {
	scratchDir := filepath.Join(s.scratchBase, jobID, attemptID)
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		return nil, "", fmt.Errorf("staging: create scratch dir %q: %w", scratchDir, err)
	}
	rules, err := s.copyInEntries(ctx, scratchDir, entries)
	if err != nil {
		s.Cleanup(scratchDir)
		return nil, "", err
	}
	return rules, scratchDir, nil
}

// copyInEntries iterates entries and copies IN/INOUT files into scratchDir,
// returning the resulting path-map rules. Each entry is placed under a
// per-index subdirectory (<scratchDir>/<i>/<basename>) so entries with the same
// basename never collide; the same index is used by StageOut for copy-back.
// Extracted to keep StageIn under the cyclop complexity limit.
func (s *Stager) copyInEntries(ctx context.Context, scratchDir string, entries []protocol.StageEntry) ([]protocol.PathMapRule, error) {
	var rules []protocol.PathMapRule
	for i, e := range entries {
		if e.Direction != "IN" && e.Direction != "INOUT" {
			continue
		}
		subDir := filepath.Join(scratchDir, strconv.Itoa(i))
		if err := os.MkdirAll(subDir, 0o750); err != nil {
			return nil, fmt.Errorf("staging: create subdir %q: %w", subDir, err)
		}
		dest := filepath.Join(subDir, filepath.Base(e.Path))
		if err := s.runSync(ctx, e.Path, dest, e.ObjectType); err != nil {
			return nil, fmt.Errorf("staging: copy-in %q: %w", e.Path, err)
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
		if err := s.runSync(ctx, src, e.Path, e.ObjectType); err != nil {
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
