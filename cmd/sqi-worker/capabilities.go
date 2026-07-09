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

		// The authoritative tag set+values: exactly what the worker would
		// advertise at boot, including manual-tag precedence (e.g. a
		// suppression like capability_tags: ["maya=false"]).
		caps, err := capabilities.BuildWorkerCapabilities(cfg.Capabilities, cfg.Worker.CapabilityTags, env)
		if err != nil {
			return fmt.Errorf("build capabilities: %w", err)
		}

		// Provenance per tag, used only for the SOURCE column: which detector
		// (builtin/custom) emitted it, or "manual"/"auto" otherwise.
		detectors, err := capabilities.LoadDetectors(cfg.Capabilities)
		if err != nil {
			return fmt.Errorf("load detectors: %w", err)
		}
		src := map[string]string{}
		for _, d := range detectors {
			for _, tag := range d.Evaluate(env) {
				if _, ok := src[tag]; !ok {
					src[tag] = d.Origin
				}
			}
		}
		for _, m := range cfg.Worker.CapabilityTags {
			key, _, _ := strings.Cut(m, "=")
			src[key] = "manual"
		}

		keys := make([]string, 0, len(caps.Tags))
		for k := range caps.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TAG\tVALUE\tSOURCE")
		for _, k := range keys {
			source, ok := src[k]
			if !ok {
				source = "auto"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", k, caps.Tags[k], source)
		}
		return w.Flush()
	},
}
