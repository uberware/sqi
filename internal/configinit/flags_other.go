// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package configinit

import "github.com/spf13/cobra"

// addUserFlag registers nothing: --user exists only in Windows builds.
func addUserFlag(*cobra.Command, *string) {}
