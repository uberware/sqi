// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
)

// ErrTemplateHasEnvironments is returned by [Capture] for a template declaring
// job or step environments.
//
// session.Manager.Create ENTERS environments, which means executing their
// onEnter actions. For a real preset that action is a vendor command
// ("setup-maya", a license checkout), and running it inside a unit test on a
// developer laptop or a CI runner is not something a snapshot should do as a
// side effect. No shipped template declares environments today; the day one
// does, that preset needs observed-only verification (Tier 3) and a deliberate
// decision, not a silent subprocess.
var ErrTemplateHasEnvironments = errors.New("presettest: template declares environments")

// ChunkPatch raises one step's chunks.defaultTaskCount before submission.
//
// It is the ONLY permitted deviation from a shipped preset's bytes, and it
// exists because chunk size is a template field rather than a job parameter:
// every SQI_CHUNK_BOUNDS preset ships defaultTaskCount 1, so without this every
// chunk golden would have Frame.Start == Frame.End and the multi-frame
// "-s START -e END" substitution those presets exist for would go untested --
// even though maya-layer-render's README tells operators to raise it.
//
// [ApplyChunkPatch] (patch.go) applies it; [Render] prints it in the golden
// header, so a patched golden can never be mistaken for the shipped preset.
type ChunkPatch struct {
	Step                   string `yaml:"step"`
	ChunksDefaultTaskCount int    `yaml:"chunks_default_task_count"`
}

// Options bounds one [Capture] call.
type Options struct {
	// Params are the job parameter values for this case. Parameters the
	// template defaults are filled in by the real binding path, so a case need
	// only state what it overrides.
	Params map[string]string

	// Format is the template format. The zero value means YAML, which is what
	// every shipped preset uses.
	Format store.TemplateFormat
}

// FileSnapshot is one resolved embedded file: its identifier, its on-disk name,
// and its body AFTER format-string or expression resolution.
type FileSnapshot struct {
	Name     string
	Filename string
	Data     string
}

// TaskSnapshot is one task's fully resolved command line.
type TaskSnapshot struct {
	Name    string
	Params  map[string]string
	Command string
	Args    []string
	Files   []FileSnapshot
}

// StepSnapshot is one step and every task it expanded to, in expansion order.
type StepSnapshot struct {
	Name  string
	Tasks []TaskSnapshot
}

// Snapshot is the whole answer for one preset and one case.
//
// Preset, Case and Patch are metadata the caller sets for the golden header;
// [Capture] populates only Params and Steps.
type Snapshot struct {
	Preset string
	Case   string
	Params map[string]string
	Patch  *ChunkPatch
	Steps  []StepSnapshot
}

// Capture expands rawTemplate under opts.Params and returns the resolved command
// line of every task, in step and expansion order.
//
// The pipeline is the production one, end to end:
//
//  1. openjd.Submitter.Submit — parameter binding, parameter-space expansion,
//     chunking, SQI_CHUNK_BOUNDS derivation, EXPR phase-2 substitution.
//  2. scheduler.BuildAssignPayload — the real assignment for each task.
//  3. session.Manager.Create + executor.ResolveAssignment — phase-3 resolution
//     of the command, args and embedded-file bodies.
//
// Nothing is reimplemented, so a golden built from this is a statement about
// the server and the worker rather than about the harness.
func Capture(ctx context.Context, rawTemplate string, opts Options) (Snapshot, error) {
	format := opts.Format
	if format == "" {
		format = store.TemplateFormatYAML
	}

	if err := refuseEnvironments(rawTemplate, format); err != nil {
		return Snapshot{}, err
	}

	st := fake.New()
	farmID, queueID, err := seedPrereqs(ctx, st)
	if err != nil {
		return Snapshot{}, err
	}

	result, err := openjd.NewSubmitter(st).Submit(ctx, rawTemplate, format, openjd.SubmitOptions{
		FarmID:     farmID,
		QueueID:    queueID,
		Owner:      "presettest",
		Parameters: opts.Params,
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("presettest: submit: %w", err)
	}

	mgr, cleanupRoot, err := newSessionManager()
	if err != nil {
		return Snapshot{}, err
	}
	defer cleanupRoot()

	snap := Snapshot{Params: result.BoundParameters}
	for _, step := range result.Steps {
		ss := StepSnapshot{Name: step.Name}
		for _, task := range result.Tasks {
			if task.StepID != step.ID {
				continue
			}
			ts, terr := captureTask(ctx, st, mgr, result, step, task, queueID)
			if terr != nil {
				return Snapshot{}, terr
			}
			ss.Tasks = append(ss.Tasks, ts)
		}
		snap.Steps = append(snap.Steps, ss)
	}
	return snap, nil
}

// refuseEnvironments parses rawTemplate and rejects it if any job or step
// environment is declared. See [ErrTemplateHasEnvironments].
func refuseEnvironments(rawTemplate string, format store.TemplateFormat) error {
	pf := openjd.FormatYAML
	if format == store.TemplateFormatJSON {
		pf = openjd.FormatJSON
	}
	tmpl, err := openjd.Parse([]byte(rawTemplate), pf)
	if err != nil {
		return fmt.Errorf("presettest: parse template: %w", err)
	}
	if len(tmpl.JobEnvironments) > 0 {
		return fmt.Errorf("%w: jobEnvironments", ErrTemplateHasEnvironments)
	}
	for _, s := range tmpl.Steps {
		if len(s.StepEnvironments) > 0 {
			return fmt.Errorf("%w: step %q stepEnvironments", ErrTemplateHasEnvironments, s.Name)
		}
	}
	return nil
}

// seedPrereqs creates the farm and queue rows Submit requires.
func seedPrereqs(ctx context.Context, st *fake.Store) (farmID, queueID string, err error) {
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "presettest-farm"})
	if err != nil {
		return "", "", fmt.Errorf("presettest: create farm: %w", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{
		ID:     uuid.NewString(),
		FarmID: farm.ID,
		Name:   "presettest-queue",
	})
	if err != nil {
		return "", "", fmt.Errorf("presettest: create queue: %w", err)
	}
	return farm.ID, queue.ID, nil
}

// newSessionManager returns a session manager rooted in a temp directory and the
// function that removes it.
//
// isolation.NewFake(nil) is the right provider precisely because it knows no
// accounts: no shipped preset's assignment carries an IsolationSpec here (the
// queue below has no run_as_user), so the provider is never asked to resolve
// one, and a future change that starts asking will fail loudly instead of
// attempting a real setuid inside a test.
func newSessionManager() (*session.Manager, func(), error) {
	root, err := os.MkdirTemp("", "presettest-sessions-*")
	if err != nil {
		return nil, nil, fmt.Errorf("presettest: temp session root: %w", err)
	}
	mgr := session.NewManager(
		filepath.Join(root, "sessions"), false,
		isolation.NewFake(nil), workerconfig.IsolationConfig{},
		fmtres.DefaultExprLimits(), slog.New(slog.DiscardHandler),
	)
	return mgr, func() { _ = os.RemoveAll(root) }, nil
}

// captureTask builds one task's assignment and resolves it.
func captureTask(
	ctx context.Context,
	st *fake.Store,
	mgr *session.Manager,
	result *openjd.SubmitResult,
	step store.Step,
	task store.Task,
	queueID string,
) (TaskSnapshot, error) {
	worker := store.Worker{
		ID:              uuid.NewString(),
		FarmID:          result.Job.FarmID,
		Hostname:        "presettest-worker",
		Status:          store.WorkerStatusOnline,
		ComputeLocation: "",
	}
	queue := store.Queue{ID: queueID, FarmID: result.Job.FarmID, Name: "presettest-queue"}

	payload, err := scheduler.BuildAssignPayload(
		ctx, task, worker, result.Job, step, queue, uuid.NewString(), st,
	)
	if err != nil {
		return TaskSnapshot{}, fmt.Errorf("presettest: build assignment for task %q: %w", task.Name, err)
	}
	var msg protocol.AssignMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return TaskSnapshot{}, fmt.Errorf("presettest: decode assignment: %w", err)
	}

	sess, err := mgr.Create(ctx, &msg)
	if err != nil {
		return TaskSnapshot{}, fmt.Errorf("presettest: create session for task %q: %w", task.Name, err)
	}
	defer mgr.Cleanup(ctx, sess, false)

	action, _, files, err := executor.ResolveAssignment(&msg, sess)
	if err != nil {
		return TaskSnapshot{}, fmt.Errorf("presettest: resolve task %q: %w", task.Name, err)
	}

	ts := TaskSnapshot{
		Name:    task.Name,
		Params:  sortedCopy(task.Parameters),
		Command: action.Command,
		Args:    action.Args,
	}
	for _, f := range files {
		ts.Files = append(ts.Files, FileSnapshot{Name: f.Name, Filename: f.Filename, Data: f.Data})
	}
	sort.Slice(ts.Files, func(i, j int) bool { return ts.Files[i].Name < ts.Files[j].Name })
	return ts, nil
}

// sortedCopy returns a copy of m. Maps render in sorted key order (see
// render.go), so this exists only to avoid aliasing the store's row.
func sortedCopy(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}
