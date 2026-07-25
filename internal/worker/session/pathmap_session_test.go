// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestCreate_WritesPathMappingFileAndExposesSessionVars verifies that when an
// assignment carries path-mapping rules, session creation writes the conformant
// pathmapping-1.0 file and that an environment onEnter action can resolve both
// {{Session.HasPathMappingRules}} (→ "true") and {{Session.PathMappingRulesFile}}
// (→ the absolute path of the written file, which exists and contains the
// conformant envelope).
func TestCreate_WritesPathMappingFileAndExposesSessionVars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		PathMap: []protocol.PathMapRule{
			{SourcePathFormat: "POSIX", SourcePath: "/nfs/projects", DestinationPath: "/mnt/nas/projects"},
		},
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args: []string{
						"-c",
						"echo {{Session.HasPathMappingRules}} > has.txt; " +
							"echo {{Session.PathMappingRulesFile}} > file.txt",
					},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Session.HasPathMappingRules resolved to "true".
	has, err := os.ReadFile(filepath.Join(s.WorkDir, "has.txt"))
	if err != nil {
		t.Fatalf("read has.txt: %v", err)
	}
	if strings.TrimSpace(string(has)) != "true" {
		t.Errorf("Session.HasPathMappingRules resolved to %q; want %q", strings.TrimSpace(string(has)), "true")
	}

	// Session.PathMappingRulesFile resolved to the written file's absolute path.
	fileBytes, err := os.ReadFile(filepath.Join(s.WorkDir, "file.txt"))
	if err != nil {
		t.Fatalf("read file.txt: %v", err)
	}
	resolvedPath := strings.TrimSpace(string(fileBytes))
	wantPath := filepath.Join(s.WorkDir, pathmap.PathMappingFileName)
	if resolvedPath != wantPath {
		t.Errorf("Session.PathMappingRulesFile = %q; want %q", resolvedPath, wantPath)
	}
	if resolvedPath != s.PathMappingRulesFile() {
		t.Errorf("resolved path %q != Session.PathMappingRulesFile() %q", resolvedPath, s.PathMappingRulesFile())
	}
	if !s.HasPathMappingRules() {
		t.Error("HasPathMappingRules() = false; want true")
	}

	// The file at that path must exist and contain the conformant envelope.
	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		t.Fatalf("path-mapping file does not exist at resolved path: %v", err)
	}
	var doc struct {
		Version          string `json:"version"`
		PathMappingRules []struct {
			SourcePathFormat string `json:"source_path_format"`
			SourcePath       string `json:"source_path"`
			DestinationPath  string `json:"destination_path"`
		} `json:"path_mapping_rules"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal path-mapping file: %v — raw: %s", err, data)
	}
	if doc.Version != "pathmapping-1.0" {
		t.Errorf("version = %q; want %q", doc.Version, "pathmapping-1.0")
	}
	if len(doc.PathMappingRules) != 1 {
		t.Fatalf("rules len = %d; want 1", len(doc.PathMappingRules))
	}
	r := doc.PathMappingRules[0]
	if r.SourcePathFormat != "POSIX" || r.SourcePath != "/nfs/projects" || r.DestinationPath != "/mnt/nas/projects" {
		t.Errorf("rule = %+v; want {POSIX /nfs/projects /mnt/nas/projects}", r)
	}
}

// TestCreate_NoPathMapExposesFalseAndNoFile verifies that when an assignment
// carries no path-mapping rules, {{Session.HasPathMappingRules}} resolves to
// "false", no file is written, and Session.PathMappingRulesFile is absent (so a
// reference to it would fail, here exercised only indirectly via the accessors).
func TestCreate_NoPathMapExposesFalseAndNoFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo {{Session.HasPathMappingRules}} > has.txt"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	has, err := os.ReadFile(filepath.Join(s.WorkDir, "has.txt"))
	if err != nil {
		t.Fatalf("read has.txt: %v", err)
	}
	if strings.TrimSpace(string(has)) != "false" {
		t.Errorf("Session.HasPathMappingRules resolved to %q; want %q", strings.TrimSpace(string(has)), "false")
	}

	if s.HasPathMappingRules() {
		t.Error("HasPathMappingRules() = true; want false")
	}
	if s.PathMappingRulesFile() != "" {
		t.Errorf("PathMappingRulesFile() = %q; want empty", s.PathMappingRulesFile())
	}

	// No path-mapping file must have been written.
	if _, err := os.Stat(filepath.Join(s.WorkDir, pathmap.PathMappingFileName)); !os.IsNotExist(err) {
		t.Errorf("expected no path-mapping file when there are no rules; stat err = %v", err)
	}
}

// TestCreate_NoTranslationFileWhenDeliveryDisabled verifies that when an
// assignment carries path-mapping rules but the PathDeliveries set does not
// include the "translation_file" delivery, no path_mapping.json is written and
// HasPathMappingRules returns false.
func TestCreate_NoTranslationFileWhenDeliveryDisabled(t *testing.T) {
	// PathMap present, but deliveries omit translation_file -> no file written.
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())
	msg := &protocol.AssignMsg{
		JobID:  "job1",
		TaskID: "task1",
		PathMap: []protocol.PathMapRule{
			{SourcePath: "/projects", DestinationPath: "/mnt/cloud", SourcePathFormat: "POSIX"},
		},
		PathDeliveries: []protocol.PathDelivery{{Kind: "swap_in_place"}}, // no translation_file
		OnRun:          &protocol.Action{Command: "true"},
	}
	sess, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.HasPathMappingRules() {
		t.Error("HasPathMappingRules = true, want false (delivery disabled)")
	}
	if _, statErr := os.Stat(filepath.Join(sess.WorkDir, "path_mapping.json")); statErr == nil {
		t.Error("path_mapping.json written despite delivery disabled")
	}
}

// TestCreate_PathMappingRulesFileReferenceFailsWithoutRules verifies that when
// there are no rules, an env action referencing {{Session.PathMappingRulesFile}}
// fails session creation cleanly (the variable is intentionally absent so the
// reference does not silently resolve to an empty path).
func TestCreate_PathMappingRulesFileReferenceFailsWithoutRules(t *testing.T) {
	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID: "j",
		Environments: []protocol.AssignEnvironment{
			{
				Name: "env-a",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo {{Session.PathMappingRulesFile}}"},
				},
			},
		},
	}

	s, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatal("Create: want error for PathMappingRulesFile reference without rules; got nil")
	}
	if s != nil {
		t.Errorf("Create must return nil session on error; got %+v", s)
	}
	if !strings.Contains(err.Error(), "Session.PathMappingRulesFile") {
		t.Errorf("error = %q; want it to name %q", err.Error(), "Session.PathMappingRulesFile")
	}
}
