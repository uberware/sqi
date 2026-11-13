// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/loadtest/report"
	"github.com/uberware/sqi/internal/loadtest/scenario"
)

func (a *app) compareCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "compare <a> <b>",
		Short: "Compare two run records metric by metric",
		Long: `Compare two run records (run.json files or run directories) metric by metric:
a difference within 5% is reported as no change.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			ra, err := report.Read(args[0])
			if err != nil {
				return err
			}
			rb, err := report.Read(args[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "A: %s\nB: %s\n\n", args[0], args[1])
			if ws := report.CompareWarnings(ra, rb); len(ws) > 0 {
				for _, w := range ws {
					fmt.Fprintf(a.stdout, "WARNING: %s\n", w)
				}
				fmt.Fprintln(a.stdout)
			}
			fmt.Fprint(a.stdout, report.RenderDeltas(report.Compare(ra, rb)))
			return nil
		},
	}
}

func (a *app) mergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge <out-dir> <shard-record>...",
		Short: "Merge the records of one run's shards into one",
		Long: `Merge the records (run.json files or run directories) of the shards of one
run into a single record written to <out-dir>. It exits like run, on the
merged record.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			recs := make([]*report.Record, 0, len(args)-1)
			for _, p := range args[1:] {
				r, err := report.Read(p)
				if err != nil {
					return err
				}
				recs = append(recs, r)
			}
			m, err := report.Merge(recs)
			if err != nil {
				return err
			}
			if err := m.Write(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "sqi-load: %s\nsqi-load: summary: %s\n", describe(m), filepath.Join(args[0], "summary.md"))
			a.code = verdict(m, false)
			return nil
		},
	}
}

// chaosMatrixFlags are the flags of scenarios --chaos-matrix.
type chaosMatrixFlags struct {
	on        bool
	os        string
	store     string
	container bool
}

func (a *app) scenariosCmd() *cobra.Command {
	var show string
	var matrix chaosMatrixFlags
	cmd := &cobra.Command{
		Use:   "scenarios [--show <name>] [--chaos-matrix --os <goos> --store <sqlite|postgres> [--container]]",
		Short: "List the built-in scenarios, or print one as resolved YAML",
		Long: `List the built-in scenarios, or with --show print one as YAML.

--chaos-matrix prints, as a JSON array, the built-in chaos-* scenarios the chaos
suite can run in the environment --os, --store and --container describe, sorted.
--os defaults to this machine's, --store to sqlite.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			switch {
			case matrix.on:
				return a.printChaosMatrix(matrix)
			case show != "":
				sc, err := scenario.Builtin(show)
				if err != nil {
					return err
				}
				out, err := yaml.Marshal(sc)
				if err != nil {
					return fmt.Errorf("encode scenario %s: %w", show, err)
				}
				_, err = a.stdout.Write(out)
				return err
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			for _, name := range scenario.BuiltinNames() {
				sc, err := scenario.Builtin(name)
				if err != nil {
					return err
				}
				fmt.Fprintf(tw, "%s\t%s\n", name, sc.Description)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&show, "show", "", "print this built-in scenario as YAML, with its defaults filled in")
	cmd.Flags().BoolVar(&matrix.on, "chaos-matrix", false, "print the chaos-* scenarios that can run in the environment --os/--store/--container describe, as a JSON array")
	cmd.Flags().StringVar(&matrix.os, "os", runtime.GOOS, "with --chaos-matrix: the GOOS of the machine running the suite")
	cmd.Flags().StringVar(&matrix.store, "store", "sqlite", "with --chaos-matrix: the store, sqlite or postgres")
	cmd.Flags().BoolVar(&matrix.container, "container", false, "with --chaos-matrix: the size-limited volumes of make chaos-container are mounted")
	return cmd
}

// printChaosMatrix writes the built-in chaos-* scenarios that m's environment
// can run as a JSON array of names.
func (a *app) printChaosMatrix(m chaosMatrixFlags) error {
	if m.store != "sqlite" && m.store != "postgres" {
		return fmt.Errorf("--store %q: want sqlite or postgres", m.store)
	}
	env := scenario.ChaosEnv{GOOS: m.os, Store: m.store, Container: m.container}
	names := []string{}
	for _, name := range scenario.BuiltinNames() {
		if !strings.HasPrefix(name, "chaos-") {
			continue
		}
		sc, err := scenario.Builtin(name)
		if err != nil {
			return err
		}
		if sc.ChaosSkipReason(env) == "" {
			names = append(names, name)
		}
	}
	out, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("encode chaos matrix: %w", err)
	}
	_, err = fmt.Fprintf(a.stdout, "%s\n", out)
	return err
}
