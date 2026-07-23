// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
)

var isolationCmd = &cobra.Command{
	Use:   "isolation",
	Short: "Manage run-as-user task isolation on this worker",
}

var setCredentialCmd = &cobra.Command{
	Use:   "set-credential <user>",
	Short: "Store the password for a run-as-user account (Windows)",
	Long: `Store the password sqi-worker uses to log on as a queue-configured
run-as-user account.

The secret is read from stdin, never from the command line — arguments are
visible to every process on the host. It is encrypted with the machine DPAPI
key and written under the worker data directory with an ACL granting only
SYSTEM and Administrators.

Machine-scope encryption is what lets an elevated Administrator provision a
credential that the LocalSystem worker service can read. It also means the
file ACL is the real boundary: anything on this host that can READ the file
can decrypt it.

Run from an elevated shell:

    sqi-worker isolation set-credential render-svc`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		user := args[0]
		cfg, err := workerconfig.Load(persistentFlags.ConfigFile, workerconfig.FlagOverrides{})
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Password for %s: ", user)
		secret, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && secret == "" {
			return fmt.Errorf("read secret: %w", err)
		}
		secret = strings.TrimRight(secret, "\r\n")
		if secret == "" {
			return errors.New("secret must not be empty")
		}

		store := isolation.NewFileStore(isolation.CredentialDir(cfg.Worker.DataDir))
		putter, ok := store.(interface {
			Put(user, secret string) error
		})
		if !ok {
			return errors.New("stored credentials are only supported on windows")
		}
		if err := putter.Put(user, secret); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "\nstored credential for %s\n", user)
		return nil
	},
}

func init() {
	isolationCmd.AddCommand(setCredentialCmd)
}
