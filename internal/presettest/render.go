// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Placeholders substituted into a rendered snapshot in place of values that
// change every run. A golden that churns is a golden nobody reviews, so every
// per-run value gets one.
const (
	workDirPlaceholder = "<WORKDIR>"
	uuidPlaceholder    = "<UUID>"
)

// uuidPattern matches a canonical UUID, which is what every id in an assignment
// is (job, step, task, attempt, worker, session).
var uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// sessionDirPattern matches the temp session root newSessionManager creates,
// including the session subdirectory beneath it.
var sessionDirPattern = regexp.MustCompile(`(?:/private)?(?:/[^\s"']*)?presettest-sessions-[0-9]+(?:/[^\s"']*)?`)

// Render returns the golden-file text for s: a header naming the preset, the
// case, the bound parameters and any template patch, then every step, every
// task, the exact command and one indexed line per argument, then the FULL
// resolved body of each embedded file.
//
// The embedded-file bodies are not optional detail. For a script-shaped preset
// the argv is merely "bash <WORKDIR>/main.sh" and every reviewable decision the
// preset makes lives in the script — a golden that stopped at argv would review
// nothing for exactly the presets that need reviewing most.
func Render(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "preset: %s\n", s.Preset)
	fmt.Fprintf(&b, "case:   %s\n", s.Case)
	if s.Patch != nil {
		fmt.Fprintf(&b, "template modified: step %q chunks.defaultTaskCount=%d\n",
			s.Patch.Step, s.Patch.ChunksDefaultTaskCount)
	}
	b.WriteString("params:\n")
	for _, k := range sortedKeys(s.Params) {
		fmt.Fprintf(&b, "  %s=%s\n", k, normalize(s.Params[k]))
	}

	for _, step := range s.Steps {
		fmt.Fprintf(&b, "\nstep %q — %s\n", step.Name, plural(len(step.Tasks), "task"))
		for i, task := range step.Tasks {
			fmt.Fprintf(&b, "\ntask %d/%d  name=%q\n", i+1, len(step.Tasks), normalize(task.Name))
			if len(task.Params) > 0 {
				b.WriteString("  task params:\n")
				for _, k := range sortedKeys(task.Params) {
					fmt.Fprintf(&b, "    %s=%s\n", k, normalize(task.Params[k]))
				}
			}
			fmt.Fprintf(&b, "  command: %s\n", normalize(task.Command))
			if len(task.Args) > 0 {
				b.WriteString("  args:\n")
				for j, a := range task.Args {
					fmt.Fprintf(&b, "    [%d]  %s\n", j, normalize(a))
				}
			}
			for _, f := range task.Files {
				fmt.Fprintf(&b, "  embedded file %q (%s):\n", f.Name, fileName(f))
				for line := range strings.SplitSeq(strings.TrimRight(normalize(f.Data), "\n"), "\n") {
					fmt.Fprintf(&b, "    %s\n", line)
				}
			}
		}
	}
	return b.String()
}

// normalize replaces per-run values with stable placeholders.
func normalize(s string) string {
	s = sessionDirPattern.ReplaceAllString(s, workDirPlaceholder)
	return uuidPattern.ReplaceAllString(s, uuidPlaceholder)
}

// fileName returns the on-disk name a resolved embedded file will have.
func fileName(f FileSnapshot) string {
	if f.Filename != "" {
		return f.Filename
	}
	return f.Name
}

// sortedKeys returns m's keys in sorted order, so a map cannot churn a golden.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// plural renders "1 task" / "3 tasks".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
