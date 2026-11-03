// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth

import "regexp"

// The NATS accounts sqi's account layout or nats-server itself defines. An
// account sqi keeps its data in must not take one of these names, or it would
// merge with or clobber that account.
const (
	// AccountAuth holds sqi's auth-callout connection.
	AccountAuth = "AUTH"
	// AccountSystem is the system account the generated cluster config
	// declares.
	AccountSystem = "SYS"
	// AccountGlobal is nats-server's built-in global account.
	AccountGlobal = "$G"
)

// accountNameRE is narrower than nats-server's grammar so that the name can
// be written into a config file unquoted and used as a directory name.
var accountNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ReservedAccount reports whether name is one of the accounts sqi or
// nats-server defines.
func ReservedAccount(name string) bool {
	return name == AccountAuth || name == AccountSystem || name == AccountGlobal
}

// ValidAccountName reports whether name has the shape sqi accepts for the
// account its data lives in: letters, digits, '_' and '-' only.
func ValidAccountName(name string) bool {
	return accountNameRE.MatchString(name)
}
