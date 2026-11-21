// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package configinit

import "errors"

// platformWriter is a placeholder until the Windows writer replaces it.
func platformWriter(_ Spec) writer {
	return func(writeRequest) error {
		return errors.New("config init is not yet supported on Windows")
	}
}
