// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"fmt"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// Broker is the NATS broker sqi-server runs on. [Embedded] hosts it inside
// the process; [Remote] connects to an operator-run server or cluster. Both
// modes share the same account layout ([Layout]) and
// authenticate workers through the same auth-callout [Responder].
type Broker interface {
	// Start brings the broker up and provisions sqi's streams. It must be
	// called once, before any other method.
	Start(ctx context.Context) error

	// Shutdown closes sqi's connections and stops the broker. It is safe to
	// call more than once and concurrently with the other methods.
	Shutdown()

	// Check implements [health.Checker] for GET /readyz.
	Check(ctx context.Context) error

	// NewClient returns a typed client on sqi's data connection credential.
	NewClient() (*Client, error)

	// Revoke disconnects every live connection of workerID. It is called
	// after the credential is revoked in the store, so the reconnect goes
	// back through the callout and is refused.
	Revoke(ctx context.Context, workerID string) error

	// RevokeBudget is how long Revoke may need to finish, slack included,
	// so a caller bounding it does not cut its final pass short. It
	// depends on the broker's auth timeout and is known after Start.
	RevokeBudget() time.Duration

	// Identity names the broker and account sqi's streams live in, so a
	// change of broker can be detected across restarts.
	Identity() string

	// AdvertiseURL is the broker URL discovery tells workers to use: the
	// one configured for advertising, else the broker's own. host is this
	// machine's discovery host name, for a broker whose URL is this machine.
	// Call it after Start.
	AdvertiseURL(host string) (string, error)
}

// AuthConfig controls worker authentication on the broker. The zero value
// leaves it off: anonymous connections are accepted into sqi's account.
type AuthConfig struct {
	// Enabled sends every connection other than sqi's own through the
	// auth-callout responder.
	Enabled bool

	// UserTTL is how long a worker's issued user JWT stays valid. The broker
	// disconnects the worker at expiry and the reconnect is authorized
	// afresh. Any positive value is accepted here; configuration enforces
	// the operator-facing bounds.
	UserTTL time.Duration

	// Lookup resolves a worker's public nkey to its worker ID from the
	// store's active credentials, at every connect.
	Lookup CredentialLookup
}

// BrokerTLSConfig controls TLS on the embedded broker's client listener.
// Zero value leaves TLS off, which is the default.
//
// This is bus's own view of the operator's nats.embedded.tls block, mapped by
// the caller, so that internal/bus does not import internal/config.
type BrokerTLSConfig struct {
	// Enabled makes the broker require TLS on every client connection.
	Enabled bool

	// CertFile and KeyFile are the PEM certificate and key the broker
	// presents. Both are validated at config load.
	CertFile string
	KeyFile  string

	// ClientCAFile, when set, requires each connecting worker to present a
	// client certificate signed by this CA. It is a transport control
	// layered on the nkey credential, never a substitute: the certificate
	// decides who may open a connection, the nkey decides which worker they
	// are.
	ClientCAFile string
}

// waitForNATS waits up to timeout for ns to accept connections. It fails as
// soon as the server reports a fatal error (a port in use, a JetStream store it
// cannot open), naming it, or when ctx ends.
func waitForNATS(ctx context.Context, ns *natsserver.Server, nl *natsLogger, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case err := <-nl.fatal:
			return fmt.Errorf("bus: start nats server: %w", err)
		case <-ctx.Done():
			return fmt.Errorf("bus: start nats server: %w", ctx.Err())
		default:
		}
		// Each call polls for up to 50 ms.
		if ns.ReadyForConnections(50 * time.Millisecond) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bus: nats server did not become ready within %s", timeout)
		}
	}
}

// inProcessURL is the URL the server's own connections to the embedded broker
// dial: none.
//
// It is deliberately empty even though nats.InProcessServer makes the URL
// unused for transport, because nats.go still PARSES it — and
// Server.ClientURL() returns a "tls://" scheme once TLS is enabled, which makes
// the client negotiate TLS over what is only a net.Pipe inside this process.
// That in turn would demand the server trust its own certificate, for a
// connection that never touches a socket.
//
// Passing no URL avoids all of it: nats-server clears info.TLSRequired for
// in-process connections (server/server.go:3296), so the connection is plain
// and authentication still applies: an in-process client presents its nkey
// and is checked like any other.
const inProcessURL = ""
