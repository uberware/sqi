// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build conformance

package conformance_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/openjd"
)

// TestConformance_TaskChunkingJobOutputs scores the TASK_CHUNKING jobs/
// fixtures, which TestConformance_Templates does not read: each runs a
// template whose one task prints "CHUNK:{{Task.Param.Frame}}", and lists
// output that must appear and output that must not. That pins how a chunk is
// spelled (CONTIGUOUS "7-8", NONCONTIGUOUS "7,8"), which a template-validity
// fixture cannot see.
//
// sqi's chunk value is the task parameter expansion yields, substituted
// unchanged, so the printed lines are "CHUNK:" plus each expanded value. A
// listed line matches as a substring, the way the fixtures' runner searches
// task output.
//
// A fixture in chunkingJobDivergences must still fail, for the same
// three-direction reason baseline.txt gives: one that starts passing is a
// stale entry.
func TestConformance_TaskChunkingJobOutputs(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(SuiteRoot, "TASK_CHUNKING", "jobs", "*.test.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no TASK_CHUNKING jobs fixtures found; is the submodule checked out?")
	}
	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			fx := loadChunkingJob(t, path)
			lines := chunkLines(t, fx.Template)
			var failures []string
			for _, want := range fx.Expected.Output {
				if !containsLine(lines, want) {
					failures = append(failures, fmt.Sprintf("no task printed %q", want))
				}
			}
			for _, bad := range fx.Expected.Forbidden {
				if containsLine(lines, bad) {
					failures = append(failures, fmt.Sprintf("a task printed forbidden %q", bad))
				}
			}
			why, baselined := chunkingJobDivergences[name]
			switch {
			case baselined && len(failures) == 0:
				t.Errorf("baselined as divergent (%s) but now passes; remove it from chunkingJobDivergences", why)
			case !baselined && len(failures) > 0:
				t.Errorf("%s; tasks printed %v", strings.Join(failures, "; "), lines)
			}
		})
	}
	for name := range chunkingJobDivergences {
		if !slices.ContainsFunc(paths, func(p string) bool { return filepath.Base(p) == name }) {
			t.Errorf("chunkingJobDivergences names %s, which is not a fixture", name)
		}
	}
}

// chunkingJobDivergences are the fixtures sqi is known to fail, each with its
// reason.
var chunkingJobDivergences = map[string]string{}

// chunkingJob is one jobs/*.test.yaml fixture.
type chunkingJob struct {
	Template yaml.Node `yaml:"template"`
	Expected struct {
		Output    []string `yaml:"output"`
		Forbidden []string `yaml:"forbidden"`
	} `yaml:"expected"`
}

func loadChunkingJob(t *testing.T, path string) chunkingJob {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fx chunkingJob
	if err := yaml.Unmarshal(data, &fx); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(fx.Expected.Output) == 0 {
		t.Fatal("fixture expects no output; nothing would be checked")
	}
	return fx
}

// chunkLines expands the fixture template's one step and returns the line each
// task prints: "CHUNK:" and its Frame value.
func chunkLines(t *testing.T, node yaml.Node) []string {
	t.Helper()
	raw, err := yaml.Marshal(&node)
	if err != nil {
		t.Fatalf("re-serialize template: %v", err)
	}
	tmpl, err := openjd.Parse(raw, openjd.FormatYAML)
	if err != nil {
		t.Fatalf("openjd.Parse: %v", err)
	}
	if len(tmpl.Steps) != 1 {
		t.Fatalf("template has %d steps, want 1", len(tmpl.Steps))
	}
	rows, err := openjd.ExpandParameterSpace(tmpl.Steps[0].ParameterSpace)
	if err != nil {
		t.Fatalf("ExpandParameterSpace: %v", err)
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = "CHUNK:" + r["Frame"]
	}
	return lines
}

func containsLine(lines []string, s string) bool {
	for _, l := range lines {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}
