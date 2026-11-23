// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"fmt"
	"strings"
)

// validateAccountArg rejects a username or group name before it is used for
// anything, including a pure-Go os/user lookup or a fake provider's map
// lookup, not only before it reaches a subprocess. what names the argument in
// error messages ("user" / "group").
//
// It is in a platform-neutral file (no build tag) so the real POSIX provider
// and the in-memory fake apply the same gate: a fake that skipped it would
// accept a "-u"-shaped username that the real provider refuses.
//
// The NUL and newline checks stop injection against tools that read
// line-oriented input; the leading-hyphen check stops id(1)/getent(1) from
// parsing the value as a flag (a queue configured with "-u" would otherwise
// change which id(1) switch runs). argv elements are never passed through a
// shell, so this is not shell escaping.
func validateAccountArg(name, what string) error {
	if name == "" {
		return fmt.Errorf("isolation: %s must not be empty", what)
	}
	if strings.ContainsAny(name, "\x00\n") {
		return fmt.Errorf("isolation: %s contains a NUL or newline byte: %q", what, name)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("isolation: %s must not start with %q: %q", what, "-", name)
	}
	return nil
}
