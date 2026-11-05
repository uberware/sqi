// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"fmt"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/loadtest/client"
)

// Credentials supplies a simulated worker's broker credential: its nkey seed
// and public key, as [WorkerSpec] carries them. An implementation must be safe
// for concurrent use, since the fleet asks for many workers' credentials at
// once.
type Credentials interface {
	// For returns the credential worker workerID connects with. A nil seed and
	// an empty public key mean the broker needs none.
	For(ctx context.Context, workerID string) (seed []byte, publicKey string, err error)
}

// NoCredentials is the credential of a server with broker authentication off:
// workers connect anonymously.
type NoCredentials struct{}

// For returns no credential.
func (NoCredentials) For(context.Context, string) (seed []byte, publicKey string, err error) {
	return nil, "", nil
}

type joinTokenCredentials struct {
	c     *client.Client
	token string
}

// JoinTokenCredentials enrolls each worker with the server by exchanging
// joinToken, the way sqi-worker's enrollment does. The token must be reusable
// (the server's join_token_single_use off), since every worker presents it. Each
// call to For generates a fresh key pair and enrolls it, with no lock shared
// between workers.
func JoinTokenCredentials(c *client.Client, joinToken string) Credentials {
	return joinTokenCredentials{c: c, token: joinToken}
}

func (j joinTokenCredentials) For(ctx context.Context, workerID string) (seed []byte, publicKey string, err error) {
	seed, publicKey, err = brokerauth.GenerateSeed()
	if err != nil {
		return nil, "", fmt.Errorf("simworker: credential for %s: %w", workerID, err)
	}
	if err := j.c.Enroll(ctx, j.token, workerID, publicKey, workerID); err != nil {
		return nil, "", fmt.Errorf("simworker: enroll %s: %w", workerID, err)
	}
	return seed, publicKey, nil
}
