// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/worker/capabilities"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

var capabilitiesCmd = &cobra.Command{
	Use:   "capabilities",
	Short: "Print the capability tags this worker would advertise",
	Long: `Run capability detection (built-in DCC detectors plus any configured
custom detectors and manual tags) and print the resulting tags with their source.
Does not connect to a server.`,
	RunE: func(_ *cobra.Command, _ []string) error {
		cfg, err := workerconfig.Load(persistentFlags.ConfigFile, workerconfig.FlagOverrides{})
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		env := capabilities.OSCheckEnv()
		detectors, err := capabilities.LoadDetectors(cfg.Capabilities)
		if err != nil {
			return fmt.Errorf("load detectors: %w", err)
		}
		// source per emitted tag
		src := map[string]string{}
		for _, d := range detectors {
			for _, tag := range d.Evaluate(env) {
				if _, ok := src[tag]; !ok {
					src[tag] = d.Origin
				}
			}
		}
		base := capabilities.Detect(nil)
		for k := range base.Tags {
			if _, ok := src[k]; !ok {
				src[k] = "auto"
			}
		}
		for _, m := range cfg.Worker.CapabilityTags {
			key, _, _ := strings.Cut(m, "=")
			src[key] = "manual"
		}
		keys := make([]string, 0, len(src))
		for k := range src {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TAG\tSOURCE")
		for _, k := range keys {
			fmt.Fprintf(w, "%s\t%s\n", k, src[k])
		}
		return w.Flush()
	},
}
