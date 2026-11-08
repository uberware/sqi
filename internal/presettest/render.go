// SPDX-License-Identifier: AGPL-3.0-or-later

package presettest

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// Placeholders substituted into a rendered snapshot in place of values that
// change every run, so a golden stays byte-stable.
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
// would leave the host-specific prefix and suffix around the redacted token.
//
// Braces end a path, as quotes and whitespace do: Modo's -cmd:@{script} {params}
// quotes each path in braces, and matching through them would swallow the
// closing brace and the second path. Square brackets end one too: the Fusion
// preset's -execute code puts its script path in a Lua long bracket,
// dofile([==[script]==]), whose closing ]==]) must stay in the golden.
var sessionDirPattern = regexp.MustCompile(`(?:[A-Za-z]:)?(?:/private)?(?:[\\/][^\s"'{}\[\]]*)?presettest-sessions-[0-9]+(?:[\\/][^\s"'{}\[\]]*)?`)

// Render returns the golden-file text for s: a header naming the preset, the
// case, the bound parameters and any template patch, then every step, every
// task, the exact command and one indexed line per argument, then the full
// resolved body of each embedded file.
//
// The embedded-file bodies are part of the golden because a script-shaped
// preset's argv is only "bash <WORKDIR>/main.sh"; the preset's decisions are in
// the script.
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
		renderLogChecks(&b, step.LogChecks)
		renderLogProgress(&b, step.LogProgress)
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

// renderLogChecks writes a step's SQI_LOG_CHECKS block, one line per pattern,
// directly under the step header. A step with no checks writes nothing.
// Patterns are quoted with %q so a regex's own backslashes and quotes are
// unambiguous. They are not normalized, since a pattern is never a path or an
// id.
func renderLogChecks(b *strings.Builder, lc *protocol.LogChecks) {
	if lc == nil {
		return
	}
	for _, p := range lc.FailOn {
		fmt.Fprintf(b, "  log checks: failOn %q\n", p)
	}
	for _, p := range lc.RequireLine {
		fmt.Fprintf(b, "  log checks: requireLine %q\n", p)
	}
}

// renderLogProgress writes a step's SQI_LOG_PROGRESS block, one line per
// pattern, after any log check lines. A step with no block writes nothing.
// Patterns are quoted with %q and not normalized, as in renderLogChecks.
func renderLogProgress(b *strings.Builder, lp *protocol.LogProgress) {
	if lp == nil {
		return
	}
	if lp.FrameParameter != "" {
		fmt.Fprintf(b, "  log progress: frameParameter %q\n", lp.FrameParameter)
	}
	for _, p := range lp.Progress {
		fmt.Fprintf(b, "  log progress: progress %q\n", p)
	}
	for _, p := range lp.Status {
		fmt.Fprintf(b, "  log progress: status %q\n", p)
	}
}

// normalize replaces per-run and per-host values with stable placeholders.
//
// hostPaths may be nil, which every POSIX host produces; see
// [hostPathReplacer].
func normalize(hostPaths *strings.Replacer, s string) string {
	if hostPaths != nil {
		s = hostPaths.Replace(s)
	}
	s = sessionDirPattern.ReplaceAllString(s, workDirPlaceholder)
	return uuidPattern.ReplaceAllString(s, uuidPlaceholder)
}

// hostPathReplacer returns a replacer that rewrites the host-flavored spelling
// of this case's own path parameters back into the POSIX spelling the goldens
// are written in, or nil when there is nothing to rewrite.
//
// A golden records what a worker runs, and phase 3 is a host context:
// Expression-Language section 1.2.1 gives SESSION and TASK scopes the host
// operating system's path semantics, so an EXPR preset resolving
// "/mnt/show/shot.mov" on a Windows worker produces "\mnt\show\shot.mov".
// executor.ResolveAssignment resolves with those semantics, so the flavor is
// undone here, in the text the golden stores, leaving one golden per case
// instead of one per GOOS.
//
// The rewrite is keyed on the case's own parameters rather than on separators
// in general, because a blanket transform would corrupt values that are not
// paths: the ffmpeg-segment-transcode-bash golden carries shell text
// ("${out//\//}") and the powershell one carries literal "C:\show\..."
// fixtures, neither of which changes with the host. Each parameter contributes
// its full value and its directory prefix (trailing separator included), so a
// sibling the preset derives in that directory ("..._seg_00000.mp4", in argv
// and in the generated concat list) converts along with it.
//
// The escaped spellings are needed because the single-backslash pattern cannot
// match inside a doubled one and would leave a half-converted path. The
// longest-first ordering below makes the escaped form win wherever both apply.
//
// On a POSIX host filepath.FromSlash is the identity, so every pair is dropped
// and this returns nil.
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
		// The same path as repr_json and repr_py write it into a script's data
		// (params.json, VRED's -postpython, Unreal's -execcmds): every
		// backslash doubled. Python's repr and JSON agree on that, and on
		// nothing else a path of ours contains, so one spelling covers both.
		if esc := strings.ReplaceAll(host, `\`, `\\`); esc != host && !seen[esc] {
			seen[esc] = true
			rewrites = append(rewrites, rewrite{host: esc, posix: posix})
		}
	}
	for _, k := range sortedKeys(params) {
		v := params[k]
		if !strings.HasPrefix(v, "/") {
			continue
		}
		// v as a full path, v as a directory, and v's parent. The directory
		// form covers a preset handed a directory that builds the filename
		// itself (ffmpeg-sequence-encode), where the joining separator needs
		// converting.
		add(v)
		add(v + "/")
		if dir, _ := path.Split(v); dir != "" && dir != "/" {
			add(dir)
		}
	}
	if len(rewrites) == 0 {
		return nil
	}
	// Longest pattern first: a replacer takes the first pattern in argument
	// order that matches at a position, so a short prefix from one parameter
	// would otherwise consume the start of a longer match belonging to another.
	// Equal lengths sort lexicographically so the result does not depend on
	// map iteration order.
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
