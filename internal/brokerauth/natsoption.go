// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth

import (
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// NkeyOption returns a [nats.Option] that authenticates a connection as the
// nkey identified by publicKey, signing the server's nonce with seed. Both
// sqi-server (its admin connection, internal/bus.Broker.adminOptions) and
// sqi-worker (internal/worker/natsclient.buildOptions) use it to present
// their own credential to the broker.
func NkeyOption(publicKey string, seed []byte) nats.Option {
	return nats.Nkey(publicKey, func(nonce []byte) ([]byte, error) {
		kp, err := nkeys.FromSeed(seed)
		if err != nil {
			return nil, err
		}
		return kp.Sign(nonce)
	})
}
