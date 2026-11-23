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
// comment. A missing file yields an empty baseline, not an error.
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
// regressions are live tests that failed but are not in the baseline. stale
// are live tests that are in the baseline but passed. orphaned are baseline
// entries that match no live result: the fixture is missing from results
// (upstream deleted or renamed it), or it is not StateLive (for example a
// base/env_templates fixture, which Classify marks StateNotApplicable). The
// regression and stale checks look only at live results, so without the
// orphan check such an entry would never be flagged.
//
// Checking all three directions keeps the baseline from becoming a permanent
// ignore-list: an entry for a passing test or for no live test must be
// removed.
//
// Not-applicable results are neither pass nor fail and are skipped for
// regressions and stale. All three slices are sorted for stable output.
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
