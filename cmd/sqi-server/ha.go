// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/ha"
)

// haFlags holds the values bound to the "ha" parent command's persistent
// flags, inherited by every subcommand.
var haFlags struct {
	DBPath string
	DSN    string
}

// haCmd groups the high-availability subcommands.
//
// Like the worker subcommands they open the state store directly, and they
// take no single-server lock, so they work with every server stopped and
// beside running ones.
var haCmd = &cobra.Command{
	Use:   "ha",
	Short: "Inspect a high-availability group of servers",
}

var haStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the servers of the high-availability group and which leads",
	Long: `Prints the leadership epoch and every server that has run against the
database, leader first, as GET /api/v1/servers lists them. A server is the
leader when it holds the unexpired leader lease, a standby when it checked in
within ha.lease_ttl, and missing otherwise.

It reads the database directly, needs no running server, and takes no lock, so
it works while every server is down and is the way to see the group when none
leads.

ha.lease_ttl is read from the configuration; use the server's own configuration
file so the judgement matches the servers'.`,
	Args: cobra.NoArgs,
	RunE: runHAStatus,
}

func init() {
	addStoreFlags(haCmd.PersistentFlags(), &haFlags.DBPath, &haFlags.DSN, "path to SQLite database file")
	haCmd.AddCommand(haStatusCmd)
}

func runHAStatus(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := loadCLIConfig()
	if err != nil {
		return err
	}
	tgt, err := c.target(cmd, haFlags.DBPath, haFlags.DSN)
	if err != nil {
		return err
	}
	// No server lock: openExistingStore opens with ServerLock false.
	st, err := openExistingStore(ctx, tgt)
	if err != nil {
		return err
	}
	defer st.Close()

	ttl := c.cfg.HA.LeaseTTL
	if ttl <= 0 {
		ttl = config.DefaultHALeaseTTL
	}
	roster, err := ha.ReadRoster(ctx, st, ttl)
	if err != nil {
		return fmt.Errorf("read the server group: %w (a database from an older release needs \"sqi-server migrate up\")", err)
	}
	return writeHAStatus(os.Stdout, c.cfg.HA.Enabled, ttl, roster)
}

// writeHAStatus prints roster to w: the HA setting, the lease TTL it was
// judged against and the epoch, then a table of the servers.
func writeHAStatus(w io.Writer, enabled bool, ttl time.Duration, roster ha.Roster) error {
	state := "not enabled in the configuration"
	if enabled {
		state = "enabled"
	}
	fmt.Fprintf(w, "HA:        %s\n", state)
	fmt.Fprintf(w, "Epoch:     %d\n", roster.Epoch)
	fmt.Fprintf(w, "Lease TTL: %s\n", ttl)
	if len(roster.Members) == 0 {
		fmt.Fprintln(w, "\nno servers have registered with this database")
		return nil
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tHOST\tVERSION\tLAST SEEN\tBROKER")
	for _, m := range roster.Members {
		broker := "ok"
		if !m.BrokerOK {
			broker = "no"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s ago\t%s\n",
			orDash(m.Name), m.Role, orDash(m.Host), orDash(m.Version), m.LastSeenAgo.Round(100*time.Millisecond), broker)
	}
	return tw.Flush()
}

// orDash is s, or "-" when it is empty, so a blank field cannot shift a
// table's columns.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
