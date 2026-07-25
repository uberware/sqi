// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"fmt"
	"strings"
)

// validateAccountArg rejects a username or group name before it is used for
// anything — including a pure-Go os/user lookup or a fake provider's map
// lookup — not just before it reaches a subprocess. what names the argument
// in error messages ("user" / "group").
//
// This lives in a platform-neutral file (no build tag) rather than under
// nss_unix.go/build:unix so both the real POSIX provider and the in-memory
// fake apply the exact same gate: a fake that skipped this check would
// accept a "-u"-shaped username that the real provider refuses, which is
// precisely the fake/real divergence isolation tests exist to catch.
//
// The NUL and newline checks close off argument-injection-adjacent tricks
// against tools that scan stdin/file lines; the leading-hyphen check stops
// the value from being parsed as a flag by id(1)/getent(1) (e.g. a queue
// misconfigured to send "-u" would otherwise silently change which id(1)
// switch runs). None of this is a shell-escaping concern — argv elements are
// never shell-interpolated — but a name id(1) parses as a flag is still a
// bypass worth closing.
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
