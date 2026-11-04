// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"path/filepath"
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

func (a *app) scenariosCmd() *cobra.Command {
	var show string
	cmd := &cobra.Command{
		Use:   "scenarios [--show <name>]",
		Short: "List the built-in scenarios, or print one as resolved YAML",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if show != "" {
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
	return cmd
}
