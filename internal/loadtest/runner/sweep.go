// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/target"
)

// maxValueName bounds the part of a sweep point's directory name taken from
// its value.
const maxValueName = 40

// Sweep runs base's scenario once for each of values, the value set at path
// (scenario.Set syntax) on top of base's own overrides, each on a fresh
// managed target, in the order given (ascending, for the knee). The sweep's
// ID is base.RunID; point i runs as <id>-p<i> and writes its run directory
// to OutDir/<id>/<i>-<value>. Every point shares base's seed, so the points
// differ only in the value. sweep.csv and sweep.md, with the knee, go to
// OutDir/<id> at the end, also after an interrupt.
//
// Every value is applied and validated before the first run, so a bad value
// fails the sweep up front. A point that cannot be set up is recorded without
// a record (the knee calls it out) and the sweep goes on; an interrupt ends
// the sweep after the point it interrupted. A sweep cannot attach: each point
// needs a server of its own.
func Sweep(ctx context.Context, base Options, path string, values []string) ([]report.SweepPoint, error) {
	scs, err := sweepPlan(&base, path, values)
	if err != nil {
		return nil, err
	}
	if err := sweepBinaries(ctx, &base); err != nil {
		return nil, err
	}
	logger := base.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	dir := filepath.Join(base.OutDir, base.RunID)
	var (
		points []report.SweepPoint
		errs   []error
	)
	for i, v := range values {
		o := base
		m := *base.Managed
		o.Managed = &m
		o.Scenario = scs[i]
		o.Mode = report.ModeSweepPoint
		o.RunID = base.RunID + "-p" + strconv.Itoa(i)
		o.Dir = filepath.Join(dir, strconv.Itoa(i)+"-"+safeName(v))
		logger.InfoContext(ctx, "sweep point", slog.String("path", path), slog.String("value", v), slog.Int("point", i), slog.Int("of", len(values)))
		rec, err := Run(ctx, o)
		if err != nil {
			errs = append(errs, fmt.Errorf("sweep point %s=%s: %w", path, v, err))
		}
		points = append(points, report.SweepPoint{Value: v, Rec: rec})
		if ctx.Err() != nil {
			break
		}
	}
	if err := report.WriteSweep(dir, path, points); err != nil {
		errs = append(errs, err)
	}
	return points, errors.Join(errs...)
}

// sweepPlan checks base and resolves the scenario of every point.
func sweepPlan(base *Options, path string, values []string) ([]*scenario.Scenario, error) {
	switch {
	case base.Attach != nil:
		return nil, errors.New("runner: a sweep runs a fresh managed server per point, so it cannot attach to one")
	case base.Scenario == nil:
		return nil, errors.New("runner: no scenario")
	case path == "" || len(values) == 0:
		return nil, errors.New("runner: a sweep needs a parameter path and at least one value")
	}
	m := target.ManagedConfig{}
	if base.Managed != nil {
		m = *base.Managed
	}
	base.Managed = &m // the caller's config is not changed
	if base.RunID == "" {
		base.RunID = NewRunID(time.Now())
	}
	if err := ValidRunID(base.RunID); err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	if base.OutDir == "" {
		base.OutDir = DefaultOutDir
	}
	if base.Seed == 0 {
		base.Seed = SeedFor(base.RunID)
	}
	return SweepScenarios(base.Scenario, path, values)
}

// SweepScenarios returns the scenario of each sweep point: a copy of base
// with the value set at path, defaults applied and validated for a managed
// target. base is not changed.
func SweepScenarios(base *scenario.Scenario, path string, values []string) ([]*scenario.Scenario, error) {
	scs := make([]*scenario.Scenario, len(values))
	for i, v := range values {
		sc := base.Clone()
		if err := sc.Set(path, v); err != nil {
			return nil, fmt.Errorf("runner: sweep value %q: %w", v, err)
		}
		sc.ApplyDefaults()
		if err := sc.Validate(scenario.TargetManaged); err != nil {
			return nil, fmt.Errorf("runner: sweep value %s=%s: %w", path, v, err)
		}
		scs[i] = sc
	}
	return scs, nil
}

// sweepBinaries builds the binaries once for every point when base was not
// given them.
func sweepBinaries(ctx context.Context, base *Options) error {
	m := base.Managed
	if m.ServerBin != "" && m.WorkerBin != "" && m.NoopBin != "" {
		return nil
	}
	server, worker, noop, err := target.BuildBinaries(ctx, filepath.Join(base.OutDir, ".bin"))
	if err != nil {
		return err
	}
	m.ServerBin, m.WorkerBin, m.NoopBin = cmpOr(m.ServerBin, server), cmpOr(m.WorkerBin, worker), cmpOr(m.NoopBin, noop)
	return nil
}

// safeName makes a sweep value usable in a directory name on every platform:
// anything but letters, digits, '.', '-' and '_' becomes '_'.
func safeName(v string) string {
	var b strings.Builder
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > maxValueName {
		s = s[:maxValueName]
	}
	// Windows drops a trailing dot from a directory name.
	if strings.HasSuffix(s, ".") {
		s = s[:len(s)-1] + "_"
	}
	return s
}
