// SPDX-License-Identifier: AGPL-3.0-or-later

package winsvc

// PathRef is one path a service depends on, with the name of where it came
// from, so a refusal can say which setting to fix.
type PathRef struct {
	// Key names the path's source: "config", "binary", or a configuration key
	// such as "nats.tls_ca_file".
	Key string
	// Path is the absolute path.
	Path string
}

// ConfigPaths is every path a loaded configuration names.
type ConfigPaths struct {
	// LogFile is log.file, "" when unset.
	LogFile string
	// Paths is every config-named path, log.file included.
	Paths []PathRef
}
