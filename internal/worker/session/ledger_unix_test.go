// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/proctree"
)

func TestRunAction_RecordsEnvironmentAction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proctrees")
	l, err := proctree.OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu  sync.Mutex
		tok string
	)
	handler := func(_, line string) string {
		mu.Lock()
		defer mu.Unlock()
		if v, ok := strings.CutPrefix(line, "tok="); ok {
			tok = v
		}
		return line
	}
	result := make(chan error, 1)
	go func() {
		result <- runAction(context.Background(), shAction(`echo "tok=$SQI_PROCESS_TREE"; sleep 1`, 0, nil),
			t.TempDir(), nil, nil, l, "s1", nopLogger(), handler)
	}()

	var files []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && len(files) == 0; time.Sleep(20 * time.Millisecond) {
		files, _ = filepath.Glob(filepath.Join(dir, "*.json")) //nolint:errcheck // the pattern is constant and valid
	}
	if len(files) != 1 {
		t.Fatalf("records while the action runs = %v, want exactly one", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Token string `json:"token"`
		Label struct {
			Kind      string `json:"kind"`
			SessionID string `json:"session_id"`
			Command   string `json:"command"`
		} `json:"label"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	if rec.Label.Kind != "environment-action" || rec.Label.SessionID != "s1" || rec.Label.Command != "sh" {
		t.Errorf("record label = %+v, want environment-action, s1, sh", rec.Label)
	}

	if err := awaitRunAction(t, result, 10*time.Second); err != nil {
		t.Fatalf("runAction: %v", err)
	}
	mu.Lock()
	got := tok
	mu.Unlock()
	if got == "" || got != rec.Token {
		t.Errorf("action saw SQI_PROCESS_TREE=%q, record token %q; want them equal and non-empty", got, rec.Token)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 { //nolint:errcheck // the pattern is constant and valid
		t.Errorf("records after the action ended: %v", left)
	}
}
