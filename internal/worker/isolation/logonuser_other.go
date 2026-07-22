// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package isolation

import (
	"context"
	"errors"
	"fmt"
)

// logonUserOS and capableOS back a logonUserProvider constructed via
// newLogonUserProvider on every platform except Windows. Production never
// reaches them here: this package's own newProvider (provider_unix.go)
// returns the POSIX unixProvider instead of a logonUserProvider. They exist,
// and fail closed rather than panicking or silently succeeding, purely so
// that logonuser.go — which carries no build tag — compiles and behaves
// safely on every platform, and so that a test which constructs a
// logonUserProvider through the real public entry point (newLogonUserProvider)
// rather than a fake seam still observes a well-defined, non-escalating
// error instead of undefined behavior.
var logonUserOS logonFunc = func(context.Context, string, string) (*Credential, error) {
	return nil, errors.New("isolation: logon_user is only implemented on windows")
}

var capableOS capableFunc = func() error {
	return fmt.Errorf("%w: logon_user is only implemented on windows", ErrNotCapable)
}
