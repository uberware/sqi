// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sqi-worker is the sqi distributed task management worker agent.
//
// It discovers and connects to a running sqi-server, registers itself with
// its capability tags and compute location, leases task assignments over core
// NATS (work.lease.<workerID>.<queueID>), and executes bare-metal OS processes inside
// OpenJD sessions.
//
// Run "sqi-worker --help" for usage.
package main

func main() {
	exitOnErr(Execute())
}
