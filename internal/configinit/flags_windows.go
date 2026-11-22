// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package configinit

import "github.com/spf13/cobra"

// addUserFlag registers --user, which only Windows builds have.
func addUserFlag(cmd *cobra.Command, user *string) {
	cmd.Flags().StringVar(user, "user", "",
		"account the service will run as (service install --user); grants it read on the --config directory and Modify on its data (and, for a worker, session) directory")
}
