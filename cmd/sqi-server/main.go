// SPDX-License-Identifier: AGPL-3.0-only

// Command sqi-server is the sqi distributed task management server.
//
// It runs the scheduler, REST API, WebSocket gateway, embedded NATS JetStream
// broker, and embedded web UI in a single binary.
//
// Run "sqi-server --help" for usage.
package main

func main() {
	exitOnErr(Execute())
}
