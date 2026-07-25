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

// DiffBaseline compares results against the baseline in both directions.
//
// regressions are live tests that failed but are not in the baseline — a real
// break. stale are live tests that are in the baseline but passed — the
// baseline needs updating.
//
// Checking both directions is what keeps the baseline from decaying into a
// permanent ignore-list: a fixed test forces its entry to be removed.
//
// Not-applicable results are skipped entirely; they are neither pass nor fail.
// Both slices are sorted for stable output.
func DiffBaseline(results []Result, baseline map[string]struct{}) (regressions, stale []string) {
	for _, r := range results {
		if r.State != StateLive {
			continue
		}
		_, listed := baseline[r.ID()]
		switch {
		case !r.Passed && !listed:
			regressions = append(regressions, r.ID())
		case r.Passed && listed:
			stale = append(stale, r.ID())
		}
	}
	sort.Strings(regressions)
	sort.Strings(stale)
	return regressions, stale
}
