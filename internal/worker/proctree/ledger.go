// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// EnvVar is the environment variable that marks every process of a tree with
// the tree's token. Descendants inherit it, so a later [Ledger.Sweep] finds a
// tree's processes by it even after the process group's leader has exited.
const EnvVar = "SQI_PROCESS_TREE"

// Label says what a recorded tree was, for the sweep's log line.
type Label struct {
	// Kind is "task" or "environment-action".
	Kind      string `json:"kind"`
	AttemptID string `json:"attempt_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Command   string `json:"command,omitempty"`
}

// record is one tree's ledger file.
type record struct {
	Token     string    `json:"token"`
	PGID      int       `json:"pgid"`
	BootID    string    `json:"boot_id"`
	Label     Label     `json:"label"`
	StartedAt time.Time `json:"started_at"`
}

// recordExt and tmpExt name a record file and the temporary file it is
// written through.
const (
	recordExt = ".json"
	tmpExt    = ".tmp"
)

// Ledger is the directory where a worker records every process tree it
// starts, so that the next worker to start on the same data directory can kill
// the trees a killed worker left running (see [Ledger.Sweep]). A nil *Ledger
// is valid and records nothing.
type Ledger struct {
	dir    string
	bootID string
}

// OpenLedger creates dir (owner-only) if needed and returns the ledger in it.
func OpenLedger(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("proctree: create ledger %s: %w", dir, err)
	}
	id, err := bootID()
	if err != nil {
		return nil, fmt.Errorf("proctree: read boot id: %w", err)
	}
	return &Ledger{dir: dir, bootID: id}, nil
}

// NewToken returns a fresh tree token: 128 random bits as lowercase hex.
func NewToken() string {
	var b [16]byte
	rand.Read(b[:]) // never fails: crypto/rand.Read crashes the program rather than return an error
	return hex.EncodeToString(b[:])
}

// MarkEnv returns env with EnvVar set to token, replacing any inherited value
// (a worker started from another worker's task inherits that task's marker). A
// nil env means the current process's environment, as it does to os/exec, so
// marking it never empties the process's environment.
func MarkEnv(env []string, token string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, EnvVar+"=") {
			out = append(out, kv)
		}
	}
	return append(out, EnvVar+"="+token)
}
