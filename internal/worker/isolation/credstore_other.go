// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package isolation

import "errors"

// fileStore is the non-Windows placeholder. Production never reaches it:
// newProvider (provider_unix.go) returns the POSIX unixProvider, which needs
// no secret because the worker must already be root. It exists so
// cmd/sqi-worker can construct a store unconditionally without build tags of
// its own, and fails closed rather than returning an empty secret that
// Resolve would then report as a confusing "empty secret" error.
type fileStore struct{}

// NewFileStore returns a CredentialStore for dir. On this platform every
// lookup fails.
func NewFileStore(_ string) CredentialStore { return &fileStore{} }

func (s *fileStore) Secret(string) (string, error) {
	return "", errors.New("isolation: stored credentials are only supported on windows")
}
