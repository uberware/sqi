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
// written (a relative path is relative to the worker's working directory),
// skipping empty keys. A command the worker itself runs (staging's sync
// command, the power commands) counts by its program when that contains a
// path separator; a bare name is found on PATH. On Windows, `service install`
// refuses a service whose named paths an unprivileged user could change:
// each one decides whom the worker trusts or what it runs.
//
// The executables jobs run (worker.commands, the capability detectors) are
// not named: job code runs as the task's account by design.
func (c WorkerConfig) NamedPaths() []NamedPath {
	all := []NamedPath{
		{"nats.tls_cert_file", c.NATS.TLSCertFile},
		{"nats.tls_key_file", c.NATS.TLSKeyFile},
		{"nats.tls_ca_file", c.NATS.TLSCAFile},
		{"nats.credential_file", c.NATS.CredentialFile},
		{"nats.join_token_file", c.NATS.JoinTokenFile},
		{"nats.server_tls_ca_file", c.NATS.ServerTLSCAFile},
		{"worker.data_dir", c.Worker.DataDir},
		{"worker.session_dir", c.Worker.SessionDir},
		{"staging.scratch_dir", c.Staging.ScratchDir},
		{"metrics.tls.cert_file", c.Metrics.TLS.CertFile},
		{"metrics.tls.key_file", c.Metrics.TLS.KeyFile},
		{"log.file", c.Log.File},
		{"staging.sync_command", commandPath(strings.Fields(c.Staging.SyncCommand))},
		{"worker.power.sleep_command", commandPath(c.Worker.Power.SleepCommand)},
		{"worker.power.shutdown_command", commandPath(c.Worker.Power.ShutdownCommand)},
	}
	return slices.DeleteFunc(all, func(p NamedPath) bool { return p.Path == "" })
}

// commandPath is argv's program when it is a path rather than a name looked
// up on PATH, else "".
func commandPath(argv []string) string {
	if len(argv) == 0 || !strings.ContainsAny(argv[0], `/\`) {
		return ""
	}
	return argv[0]
}
