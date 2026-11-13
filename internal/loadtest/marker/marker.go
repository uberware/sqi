// SPDX-License-Identifier: AGPL-3.0-or-later

// Package marker records what a load run's real tasks did, in files under a
// directory the harness owns: one exec file per process the no-op command
// started, refreshed while the process lives, and one output file per task
// key, appended to by every process that writes the task's output. The chaos
// checks read them to find a process that outlived its worker and an output
// that two attempts wrote at once.
package marker

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The roles of an exec.
const (
	RoleMain   = "main"   // the task's own process
	RoleChatty = "chatty" // a child printing to the task's stdout
	RoleSilent = "silent" // a child with no stdio of the task's
)

// The marker directory's subdirectories.
const (
	ExecsDir   = "execs"   // one <exec id>.json per process
	OutputsDir = "outputs" // <key hash>.out and <key hash>.key per task key
	LogsDir    = "logs"    // the silent children's stdio files
)

// RefreshInterval is how often a live exec's LastSeen is rewritten.
const RefreshInterval = 250 * time.Millisecond

// Exec is one process the no-op started: the main task process or a child.
type Exec struct {
	ID          string     `json:"id"`          // random, unique per process
	Key         string     `json:"key"`         // the task key, see scenario.KeyArg
	Incarnation string     `json:"incarnation"` // SQILOAD_INCARNATION
	Role        string     `json:"role"`        // "main", "chatty" or "silent"
	PID         int        `json:"pid"`
	PGID        int        `json:"pgid"` // 0 on Windows
	Start       time.Time  `json:"start"`
	LastSeen    time.Time  `json:"last_seen"` // refreshed every 250ms while alive
	End         *time.Time `json:"end,omitempty"`
	ExitCode    int        `json:"exit_code"`
}

// Writer keeps one exec file current.
type Writer struct {
	path string

	stop chan struct{}
	done chan struct{}

	mu       sync.Mutex
	exec     Exec
	finished bool
}

// NewID returns a random exec ID.
func NewID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("marker: random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Begin writes <dir>/execs/<id>.json for this process under a random ID and
// starts refreshing its LastSeen every 250ms.
func Begin(dir, key, incarnation, role string) (*Writer, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	return BeginID(dir, id, key, incarnation, role)
}

// BeginID is Begin under an ID the caller chose, as a parent that names its
// child's files before starting it does.
func BeginID(dir, id, key, incarnation, role string) (*Writer, error) {
	if !validID(id) {
		return nil, fmt.Errorf("marker: exec id %q is not letters, digits, '-' and '_'", id)
	}
	execs := filepath.Join(dir, ExecsDir)
	if err := os.MkdirAll(execs, 0o750); err != nil {
		return nil, fmt.Errorf("marker: %w", err)
	}
	now := now()
	w := &Writer{
		path: filepath.Join(execs, id+".json"),
		stop: make(chan struct{}),
		done: make(chan struct{}),
		exec: Exec{
			ID: id, Key: key, Incarnation: incarnation, Role: role,
			PID: os.Getpid(), PGID: processGroup(), Start: now, LastSeen: now,
		},
	}
	if err := w.write(true); err != nil {
		return nil, err
	}
	go w.refresh()
	return w, nil
}

// ID is the exec's ID.
func (w *Writer) ID() string { return w.exec.ID }

func (w *Writer) refresh() {
	defer close(w.done)
	t := time.NewTicker(RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.mu.Lock()
			w.exec.LastSeen = now()
			// A failed refresh is retried on the next tick; Windows refuses
			// the rename while a reader has the file open.
			w.write(false) //nolint:errcheck // see above
			w.mu.Unlock()
		}
	}
}

// Finish stops refreshing and writes End and ExitCode. Only the first call
// does anything.
func (w *Writer) Finish(exitCode int) error {
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
		return nil
	}
	w.finished = true
	w.mu.Unlock()
	close(w.stop)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	end := now()
	w.exec.LastSeen, w.exec.End, w.exec.ExitCode = end, &end, exitCode
	return w.write(true)
}

// write replaces the exec file through a temporary file in the same
// directory, retrying a failed rename when retry is set. The caller holds mu.
func (w *Writer) write(retry bool) error {
	b, err := json.Marshal(w.exec)
	if err != nil {
		return fmt.Errorf("marker: %w", err)
	}
	return replaceFile(w.path, b, retry)
}

// replaceFile writes data to path through a temporary file and a rename, so a
// reader sees the old content or the new, never part of it.
func replaceFile(path string, data []byte, retry bool) error {
	dir, base := filepath.Split(path)
	tmp := filepath.Join(dir, "."+base+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("marker: %w", err)
	}
	tries := 1
	if retry {
		tries = renameTries
	}
	var err error
	for i := range tries {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		if i+1 < tries {
			time.Sleep(renamePause)
		}
	}
	return fmt.Errorf("marker: %w", err)
}

// Rename retries, for Windows, where a rename onto a file a reader holds open
// fails until the reader closes it.
const (
	renameTries = 50
	renamePause = 10 * time.Millisecond
)

// outputName is the base name of key's output files: the first 16 hex digits
// of its SHA-256.
func outputName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// Output appends "<execID> <RFC3339Nano>\n" to
// <dir>/outputs/<hex sha256(key)[:16]>.out, and remembers key in
// <dir>/outputs/<same>.key on first use. Writers of one key may make their
// first call at once.
func Output(dir, key, execID string, at time.Time) error {
	if !validID(execID) {
		return fmt.Errorf("marker: exec id %q is not letters, digits, '-' and '_'", execID)
	}
	outputs := filepath.Join(dir, OutputsDir)
	if err := os.MkdirAll(outputs, 0o750); err != nil {
		return fmt.Errorf("marker: %w", err)
	}
	name := filepath.Join(outputs, outputName(key))
	if _, err := os.Stat(name + ".key"); errors.Is(err, fs.ErrNotExist) {
		if err := writeKey(name+".key", key, execID); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(name+".out", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("marker: %w", err)
	}
	// One write per line, so lines of concurrent writers never interleave.
	_, err = f.WriteString(execID + " " + at.UTC().Format(time.RFC3339Nano) + "\n")
	return errors.Join(err, f.Close())
}

// writeKey creates the key file at path holding key, unless another writer
// already has. The content goes to a temporary file of this writer's own
// first and is then linked into place, which fails rather than replaces when
// the file exists, so a reader never sees a key file empty or half written.
func writeKey(path, key, execID string) error {
	dir, base := filepath.Split(path)
	tmp := filepath.Join(dir, "."+base+"."+execID+".tmp")
	if err := os.WriteFile(tmp, []byte(key), 0o600); err != nil {
		return fmt.Errorf("marker: %w", err)
	}
	defer os.Remove(tmp)
	err := os.Link(tmp, path)
	if err == nil {
		return nil
	}
	if _, serr := os.Stat(path); serr == nil {
		return nil // another writer's, with the same content
	}
	// No hard links on this file system: a rename of this writer's own
	// temporary file is atomic too.
	if rerr := os.Rename(tmp, path); rerr != nil {
		if _, serr := os.Stat(path); serr == nil {
			return nil
		}
		return fmt.Errorf("marker: create %s: %w", path, errors.Join(err, rerr))
	}
	return nil
}

// ReadAll reads every exec file in dir. Unreadable or torn files are skipped
// and counted. A missing directory holds no execs.
func ReadAll(dir string) (execs []Exec, skipped int, err error) {
	entries, err := os.ReadDir(filepath.Join(dir, ExecsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("marker: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, ExecsDir, name))
		var x Exec
		if err != nil || json.Unmarshal(b, &x) != nil || x.ID == "" {
			skipped++
			continue
		}
		execs = append(execs, x)
	}
	return execs, skipped, nil
}

// OutputLine is one line of an output file.
type OutputLine struct {
	ExecID string
	At     time.Time
}

// Outputs reads every output file in dir: key -> lines in file order. A line
// cut short by a writer that died mid-write is skipped.
func Outputs(dir string) (map[string][]OutputLine, error) {
	outputs := filepath.Join(dir, OutputsDir)
	files, err := filepath.Glob(filepath.Join(outputs, "*.out"))
	if err != nil {
		return nil, fmt.Errorf("marker: %w", err)
	}
	out := map[string][]OutputLine{}
	for _, f := range files {
		name := strings.TrimSuffix(f, ".out")
		key, err := os.ReadFile(name + ".key")
		if err != nil {
			return nil, fmt.Errorf("marker: the key of %s: %w", f, err)
		}
		if len(key) == 0 {
			return nil, fmt.Errorf("marker: the key file of %s is empty", f)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("marker: %w", err)
		}
		out[string(key)] = parseLines(data)
	}
	return out, nil
}

func parseLines(data []byte) []OutputLine {
	var lines []OutputLine
	parts := strings.Split(string(data), "\n")
	// The last part follows the last newline: empty, or a line its writer
	// did not finish.
	for _, line := range parts[:len(parts)-1] {
		id, at, ok := strings.Cut(line, " ")
		t, err := time.Parse(time.RFC3339Nano, at)
		if !ok || id == "" || err != nil {
			continue
		}
		lines = append(lines, OutputLine{ExecID: id, At: t})
	}
	return lines
}

// validID reports whether id is safe as a file name.
func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		ok := r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

// now is the wall-clock time without its monotonic reading, as it is after
// a round trip through JSON.
func now() time.Time { return time.Now().Round(0) }
