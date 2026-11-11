// SPDX-License-Identifier: AGPL-3.0-or-later

package marker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// only returns the one exec ReadAll finds in dir.
func only(t *testing.T, dir string) Exec {
	t.Helper()
	execs, skipped, err := ReadAll(dir)
	if err != nil || skipped != 0 || len(execs) != 1 {
		t.Fatalf("ReadAll = %+v, skipped %d, %v; want one exec", execs, skipped, err)
	}
	return execs[0]
}

func TestBegin_RefreshesAndFinishes(t *testing.T) {
	dir := t.TempDir()
	w, err := Begin(dir, "j/step-1/1", "0.0", RoleMain)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	first := only(t, dir)
	if first.ID == "" || first.ID != w.ID() || first.Key != "j/step-1/1" || first.Incarnation != "0.0" || first.Role != RoleMain {
		t.Errorf("exec = %+v, want id %s, key j/step-1/1, incarnation 0.0, role main", first, w.ID())
	}
	if first.PID != os.Getpid() || first.Start.IsZero() || first.End != nil {
		t.Errorf("exec = %+v, want this process, started, no end", first)
	}
	time.Sleep(600 * time.Millisecond)
	later := only(t, dir)
	if !later.LastSeen.After(first.LastSeen) {
		t.Errorf("LastSeen %s did not advance from %s across 600ms", later.LastSeen, first.LastSeen)
	}
	if err := w.Finish(3); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	done := only(t, dir)
	if done.End == nil || done.ExitCode != 3 {
		t.Fatalf("after Finish(3) exec = %+v, want an end and exit code 3", done)
	}
	time.Sleep(600 * time.Millisecond)
	if after := only(t, dir); !after.LastSeen.Equal(done.LastSeen) {
		t.Errorf("LastSeen advanced after Finish: %s -> %s", done.LastSeen, after.LastSeen)
	}
	if err := w.Finish(0); err != nil {
		t.Errorf("a second Finish: %v", err)
	}
	if again := only(t, dir); again.ExitCode != 3 {
		t.Errorf("a second Finish rewrote the exit code to %d", again.ExitCode)
	}
}

func TestBegin_IDsAreUnique(t *testing.T) {
	dir := t.TempDir()
	a, err := Begin(dir, "k", "0.0", RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Finish(0) //nolint:errcheck // test cleanup
	b, err := Begin(dir, "k", "0.0", RoleChatty)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Finish(0) //nolint:errcheck // test cleanup
	if a.ID() == b.ID() {
		t.Errorf("two Begins share id %s", a.ID())
	}
}

func TestBeginID_RejectsAnUnsafeID(t *testing.T) {
	for _, id := range []string{"", "../x", "a/b", `a\b`, "."} {
		if w, err := BeginID(t.TempDir(), id, "k", "0.0", RoleSilent); err == nil {
			w.Finish(0) //nolint:errcheck // test cleanup
			t.Errorf("BeginID(%q) succeeded", id)
		}
	}
}

func TestReadAll_SkipsTornFiles(t *testing.T) {
	dir := t.TempDir()
	w, err := Begin(dir, "k", "0.0", RoleMain)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(0); err != nil {
		t.Fatal(err)
	}
	execs := filepath.Join(dir, ExecsDir)
	if err := os.WriteFile(filepath.Join(execs, "torn.json"), []byte(`{"id":"torn","key":"k`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(execs, "empty.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A temporary file of a write in progress is not an exec file.
	if err := os.WriteFile(filepath.Join(execs, ".x.json.tmp"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := ReadAll(dir)
	if err != nil || skipped != 2 || len(got) != 1 || got[0].ID != w.ID() {
		t.Errorf("ReadAll = %+v, skipped %d, %v; want the one valid exec and 2 skipped", got, skipped, err)
	}
}

func TestReadAll_NoDirectory(t *testing.T) {
	execs, skipped, err := ReadAll(filepath.Join(t.TempDir(), "absent"))
	if err != nil || skipped != 0 || len(execs) != 0 {
		t.Errorf("ReadAll of a missing directory = %+v, %d, %v; want nothing", execs, skipped, err)
	}
}

func TestOutput_AppendsAndReads(t *testing.T) {
	dir := t.TempDir()
	const key = "job-1/step-1/4"
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	writes := []OutputLine{
		{ExecID: "a", At: base},
		{ExecID: "b", At: base.Add(time.Millisecond)},
		{ExecID: "a", At: base.Add(2 * time.Millisecond)},
		{ExecID: "b", At: base.Add(3 * time.Millisecond)},
	}
	for _, l := range writes {
		if err := Output(dir, key, l.ExecID, l.At); err != nil {
			t.Fatalf("Output: %v", err)
		}
	}
	if err := Output(dir, "other", "c", base); err != nil {
		t.Fatal(err)
	}
	outs, err := Outputs(dir)
	if err != nil {
		t.Fatalf("Outputs: %v", err)
	}
	if len(outs) != 2 || len(outs["other"]) != 1 {
		t.Fatalf("Outputs = %+v, want two keys", outs)
	}
	got := outs[key]
	if len(got) != len(writes) {
		t.Fatalf("Outputs[%s] = %+v, want %d lines", key, got, len(writes))
	}
	for i, l := range got {
		if l.ExecID != writes[i].ExecID || !l.At.Equal(writes[i].At) {
			t.Errorf("line %d = %+v, want %+v", i, l, writes[i])
		}
	}
}

func TestOutputs_SkipsATornLine(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := Output(dir, "k", "a", at); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, OutputsDir, outputName("k")+".out"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("b 2026-11"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	outs, err := Outputs(dir)
	if err != nil || len(outs["k"]) != 1 {
		t.Errorf("Outputs = %+v, %v; want the one whole line", outs, err)
	}
}
