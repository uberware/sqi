// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"slices"
	"strings"
)

// NamedPath is one file or directory a configuration names, with the dotted
// YAML key that names it.
type NamedPath struct{ Key, Path string }

// NamedPaths returns every file and directory the configuration names, as
// written (a relative path is relative to the server's working directory),
// skipping empty keys. A capacity provider's program counts when it contains
// a path separator; a bare name is found on PATH. On Windows, `service
// install` refuses a service whose named paths an unprivileged user could
// change: each one decides whom the server trusts or what it runs.
func (c Config) NamedPaths() []NamedPath {
	all := []NamedPath{
		{"http.tls.cert_file", c.HTTP.TLS.CertFile},
		{"http.tls.key_file", c.HTTP.TLS.KeyFile},
		{"nats.embedded.data_dir", c.NATS.Embedded.DataDir},
		{"nats.embedded.tls.cert_file", c.NATS.Embedded.TLS.CertFile},
		{"nats.embedded.tls.key_file", c.NATS.Embedded.TLS.KeyFile},
		{"nats.embedded.tls.client_ca_file", c.NATS.Embedded.TLS.ClientCAFile},
		{"nats.external.nkey_seed_file", c.NATS.External.NkeySeedFile},
		{"nats.external.creds_file", c.NATS.External.CredsFile},
		{"nats.external.tls.ca_file", c.NATS.External.TLS.CAFile},
		{"nats.external.tls.cert_file", c.NATS.External.TLS.CertFile},
		{"nats.external.tls.key_file", c.NATS.External.TLS.KeyFile},
		{"nats.external.callout.nkey_seed_file", c.NATS.External.Callout.NkeySeedFile},
		{"nats.external.callout.issuer_seed_file", c.NATS.External.Callout.IssuerSeedFile},
		{"nats.external.callout.system_creds_file", c.NATS.External.Callout.SystemCredsFile},
		{"store.sqlite_path", c.Store.SQLitePath},
		{"log.file", c.Log.File},
		{"auth.ldap.ca_file", c.Auth.LDAP.CAFile},
	}
	names := make([]string, 0, len(c.Capacity.Providers))
	for name := range c.Capacity.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if cmd := c.Capacity.Providers[name].Command; len(cmd) > 0 && strings.ContainsAny(cmd[0], `/\`) {
			all = append(all, NamedPath{"capacity.providers." + name + ".command", cmd[0]})
		}
	}
	return slices.DeleteFunc(all, func(p NamedPath) bool { return p.Path == "" })
}
