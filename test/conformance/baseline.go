// SPDX-License-Identifier: AGPL-3.0-or-later

package conformance

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// LoadBaseline reads a baseline file: one known-failing test ID per line.
// Blank lines are ignored, and everything from a '#' to end of line is a
// comment. A missing file yields an empty baseline, not an error — that is the
// legitimate starting state before the first run.
func LoadBaseline(path string) (map[string]struct{}, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open baseline: %w", err)
	}
	defer func() { _ = f.Close() }()

	out := map[string]struct{}{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			out[line] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read baseline: %w", err)
	}
	return out, nil
}

// DiffBaseline compares results against the baseline in three directions.
//
// regressions are live tests that failed but are not in the baseline — a real
// break. stale are live tests that are in the baseline but passed — the
// baseline needs updating. orphaned are baseline entries that match no live
// result at all: either the fixture no longer exists in results (upstream
// deleted or renamed it), or it exists but is no longer StateLive (for
// example a fixture reclassified to StateNotApplicable, as base/env_templates
// was by Finding 1's kind-aware Classify). Without this check such an entry
// is invisible forever — the regression/stale loop below only ever looks at
// live results, so a line nothing live ever matches is never flagged by
// either of the other two directions.
//
// Checking all three directions is what keeps the baseline from decaying into
// a permanent ignore-list: a fixed test forces its entry to be removed, and a
// dead entry is forced out too instead of rotting silently.
//
// Not-applicable results are otherwise skipped entirely for regressions/stale;
// they are neither pass nor fail. All three slices are sorted for stable
// output.
func DiffBaseline(results []Result, baseline map[string]struct{}) (regressions, stale, orphaned []string) {
	live := make(map[string]struct{}, len(results))
	for _, r := range results {
		if r.State != StateLive {
			continue
		}
		live[r.ID()] = struct{}{}

		_, listed := baseline[r.ID()]
		switch {
		case !r.Passed && !listed:
			regressions = append(regressions, r.ID())
		case r.Passed && listed:
			stale = append(stale, r.ID())
		}
	}

	for id := range baseline {
		if _, ok := live[id]; !ok {
			orphaned = append(orphaned, id)
		}
	}

	sort.Strings(regressions)
	sort.Strings(stale)
	sort.Strings(orphaned)
	return regressions, stale, orphaned
}
