// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package isolation

import (
	"context"
	"errors"
	"testing"
)

// TestLogonUserProvider_NonWindowsStubFailsClosed proves the actual
// PRODUCTION wiring behind newLogonUserProvider — not a test-injected seam —
// fails closed on every platform except Windows. Nothing in this package's
// own newProvider (provider_unix.go) ever constructs a logonUserProvider on
// POSIX, so this exists purely as a belt-and-braces guard: if a future call
// site ever did reach logonUserOS/capableOS here, the result must be an
// explicit error, never silent, unisolated success.
func TestLogonUserProvider_NonWindowsStubFailsClosed(t *testing.T) {
	p := newLogonUserProvider(fakeStore{"render-svc": "s3cr3t"})
	if _, err := p.Resolve(context.Background(), Spec{User: "render-svc"}); err == nil {
		t.Error("Resolve via the real (non-Windows) seam must fail, never silently succeed")
	}
}

// TestLogonUserProvider_NonWindowsCapableFailsClosed is the Capable()
// counterpart of the above.
func TestLogonUserProvider_NonWindowsCapableFailsClosed(t *testing.T) {
	p := newLogonUserProvider(fakeStore{})
	if err := p.Capable(); !errors.Is(err, ErrNotCapable) {
		t.Errorf("Capable() = %v, want ErrNotCapable", err)
	}
}
