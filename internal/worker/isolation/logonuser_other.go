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
// reaches them here: newProvider (provider_unix.go) returns the POSIX
// unixProvider instead of a logonUserProvider. They exist so logonuser.go,
// which carries no build tag, compiles on every platform, and they fail
// closed, so a test that constructs a logonUserProvider through
// newLogonUserProvider rather than a fake seam gets a well-defined error.
var logonUserOS logonFunc = func(context.Context, string, string) (*Credential, error) {
	return nil, errors.New("isolation: logon_user is only implemented on windows")
}

var capableOS capableFunc = func() error {
	return fmt.Errorf("%w: logon_user is only implemented on windows", ErrNotCapable)
}

// canTraverseOS backs the traverse check on every platform except Windows.
// Production never reaches it (newProvider returns the POSIX unixProvider);
// it reports true so a test constructing a logonUserProvider through the real
// constructor is not blocked by a check that has no meaning here.
var canTraverseOS traverseFunc = func(*Credential) (bool, error) { return true, nil }
