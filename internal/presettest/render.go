// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"fmt"
	"path"
	"path/filepath"
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
//
// Both separators and an optional drive letter are matched because the temp
// root is the host's own: POSIX gives "/tmp/presettest-sessions-1/...", Windows
// gives "C:\Users\...\Temp\presettest-sessions-1\...". Matching only "/"
// would redact the bare "presettest-sessions-1" token and leave the volatile
// prefix and suffix around it in the golden.
var sessionDirPattern = regexp.MustCompile(`(?:[A-Za-z]:)?(?:/private)?(?:[\\/][^\s"']*)?presettest-sessions-[0-9]+(?:[\\/][^\s"']*)?`)

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
	hostPaths := hostPathReplacer(s.Params)

	var b strings.Builder
	fmt.Fprintf(&b, "preset: %s\n", s.Preset)
	fmt.Fprintf(&b, "case:   %s\n", s.Case)
	if s.Patch != nil {
		fmt.Fprintf(&b, "template modified: step %q chunks.defaultTaskCount=%d\n",
			s.Patch.Step, s.Patch.ChunksDefaultTaskCount)
	}
	b.WriteString("params:\n")
	for _, k := range sortedKeys(s.Params) {
		fmt.Fprintf(&b, "  %s=%s\n", k, normalize(hostPaths, s.Params[k]))
	}

	for _, step := range s.Steps {
		fmt.Fprintf(&b, "\nstep %q — %s\n", step.Name, plural(len(step.Tasks), "task"))
		for i, task := range step.Tasks {
			fmt.Fprintf(&b, "\ntask %d/%d  name=%q\n", i+1, len(step.Tasks), normalize(hostPaths, task.Name))
			if len(task.Params) > 0 {
				b.WriteString("  task params:\n")
				for _, k := range sortedKeys(task.Params) {
					fmt.Fprintf(&b, "    %s=%s\n", k, normalize(hostPaths, task.Params[k]))
				}
			}
			fmt.Fprintf(&b, "  command: %s\n", normalize(hostPaths, task.Command))
			if len(task.Args) > 0 {
				b.WriteString("  args:\n")
				for j, a := range task.Args {
					fmt.Fprintf(&b, "    [%d]  %s\n", j, normalize(hostPaths, a))
				}
			}
			for _, f := range task.Files {
				fmt.Fprintf(&b, "  embedded file %q (%s):\n", f.Name, fileName(f))
				for line := range strings.SplitSeq(strings.TrimRight(normalize(hostPaths, f.Data), "\n"), "\n") {
					fmt.Fprintf(&b, "    %s\n", line)
				}
			}
		}
	}
	return b.String()
}

// normalize replaces per-run and per-host values with stable placeholders.
//
// hostPaths may be nil, which is what every POSIX host produces; see
// [hostPathReplacer].
func normalize(hostPaths *strings.Replacer, s string) string {
	if hostPaths != nil {
		s = hostPaths.Replace(s)
	}
	s = sessionDirPattern.ReplaceAllString(s, workDirPlaceholder)
	return uuidPattern.ReplaceAllString(s, uuidPlaceholder)
}

// hostPathReplacer returns a replacer that rewrites the HOST-flavored spelling
// of this case's own path parameters back into the POSIX spelling the goldens
// are written in, or nil when there is nothing to rewrite.
//
// A golden records what a worker really runs, and phase 3 is a host context:
// Expression-Language section 1.2.1 gives SESSION and TASK scopes the host
// operating system's path semantics, so an EXPR preset resolving
// "/mnt/show/shot.mov" on a Windows worker really does produce
// "\mnt\show\shot.mov". That is correct, and pinning POSIX at resolution
// time would make the golden show something no Windows worker produces --
// executor.ResolveAssignment exists precisely so the golden cannot drift from
// what the worker does.
//
// So the flavor is undone HERE, at the text the golden stores, leaving one
// reviewed artifact per case instead of one per GOOS. The rewrite is keyed on
// the case's own parameters rather than on separators generally, because a
// blanket transform would corrupt the values that are not paths: the
// ffmpeg-segment-transcode-bash golden carries shell text ("${out//\//}")
// and the powershell one carries literal "C:\show\..." fixtures, neither of
// which changes with the host. Each parameter contributes its full value and
// its directory prefix (trailing separator included), so a sibling the preset
// DERIVES in that directory -- "..._seg_00000.mp4", which appears in argv and
// inside the generated concat list -- converts along with it.
//
// On a POSIX host filepath.FromSlash is the identity, so every pair is dropped
// and this returns nil: the rendered text there is byte-for-byte what it was
// before this existed.
func hostPathReplacer(params map[string]string) *strings.Replacer {
	type rewrite struct{ host, posix string }
	var rewrites []rewrite
	seen := make(map[string]bool)
	add := func(posix string) {
		host := filepath.FromSlash(posix)
		if host == posix || seen[host] {
			return
		}
		seen[host] = true
		rewrites = append(rewrites, rewrite{host: host, posix: posix})
	}
	for _, k := range sortedKeys(params) {
		v := params[k]
		if !strings.HasPrefix(v, "/") {
			continue
		}
		// v as a full path, v as a DIRECTORY, and v's parent. The middle one
		// matters because a preset may be handed a directory and build the
		// filename itself -- ffmpeg-sequence-encode is exactly that -- so the
		// separator the join contributes is the one that needs converting.
		add(v)
		add(v + "/")
		if dir, _ := path.Split(v); dir != "" && dir != "/" {
			add(dir)
		}
	}
	if len(rewrites) == 0 {
		return nil
	}
	// Longest pattern first, and NOT in parameter order: a replacer takes the
	// first pattern in ARGUMENT order that matches at a position, so a short
	// prefix contributed by one parameter would otherwise consume the start of
	// a longer, more exact match belonging to another. Sorting by descending
	// length makes the most specific parameter win wherever two overlap;
	// equal lengths sort lexicographically so the result cannot depend on map
	// iteration order.
	sort.Slice(rewrites, func(i, j int) bool {
		if len(rewrites[i].host) != len(rewrites[j].host) {
			return len(rewrites[i].host) > len(rewrites[j].host)
		}
		return rewrites[i].host < rewrites[j].host
	})
	pairs := make([]string, 0, len(rewrites)*2)
	for _, r := range rewrites {
		pairs = append(pairs, r.host, r.posix)
	}
	return strings.NewReplacer(pairs...)
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
