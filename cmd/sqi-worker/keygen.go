// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/brokerauth"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

// workerCredentialFilename is the seed filename under the worker data
// directory, matching internal/worker/config's default for
// NATS.CredentialFile (<worker.data_dir>/worker.nk).
const workerCredentialFilename = "worker.nk"

var keygenFlags struct {
	DataDir string
	Force   bool
}

var keygenCmd = &cobra.Command{
	Use:   "keygen",
	Short: "Generate a new nkey broker credential for this worker",
	Long: `Generate a new Ed25519 nkey keypair for authenticating this worker to
sqi-server's NATS broker, and write the private seed to
<data-dir>/worker.nk (mode 0600).

Refuses to overwrite an existing seed unless --force is given — generating a
new keypair for a worker that is already enrolled invalidates its current
credential; the broker will reject connections signed with the old key until
the worker is re-enrolled with the new public key.

Prints the public key and the exact "sqi-server worker enroll" command an
operator must run on the server to authorize this worker. This is the manual
enrollment path — a worker that can reach sqi-server's REST API can instead
enroll itself automatically with a join token issued by
"sqi-server worker token issue".`,
	RunE: runKeygen,
}

func init() {
	keygenCmd.Flags().StringVar(
		&keygenFlags.DataDir,
		"data-dir", workerconfig.Default().Worker.DataDir,
		"worker data directory (the seed is written to <data-dir>/worker.nk)",
	)
	keygenCmd.Flags().BoolVar(
		&keygenFlags.Force,
		"force", false,
		"overwrite an existing seed file",
	)
}

func runKeygen(_ *cobra.Command, _ []string) error {
	dataDir := keygenFlags.DataDir
	if dataDir == "" {
		return errors.New("data directory is empty; use --data-dir")
	}
	seedPath := filepath.Join(dataDir, workerCredentialFilename)

	_, statErr := os.Stat(seedPath)
	switch {
	case statErr == nil:
		if !keygenFlags.Force {
			return fmt.Errorf(
				"a credential already exists at %s; use --force to overwrite it (this invalidates the existing enrollment)",
				seedPath,
			)
		}
	case errors.Is(statErr, os.ErrNotExist):
		// No existing seed — nothing to warn about below.
	default:
		return fmt.Errorf("stat %s: %w", seedPath, statErr)
	}
	seedExisted := statErr == nil

	seed, publicKey, err := brokerauth.GenerateSeed()
	if err != nil {
		return fmt.Errorf("generate keypair: %w", err)
	}

	if err := brokerauth.SaveSeed(seedPath, seed); err != nil {
		return err
	}

	workerID, err := workerconfig.LoadOrCreateWorkerID(dataDir)
	if err != nil {
		return fmt.Errorf("load or create worker id: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Public key: %s\n", publicKey)
	fmt.Fprintln(os.Stdout, "On the server, run:")
	fmt.Fprintf(os.Stdout, "  sqi-server worker enroll --worker-id %s --public-key %s\n", workerID, publicKey)

	if seedExisted {
		// --force just overwrote a seed that may still be the credential the
		// server has enrolled. The new local key means nothing to the broker
		// until the server side is updated too — and today the server still
		// has the OLD public key on file, so it must be revoked before the
		// enroll command above can succeed (worker_id stays unique among
		// active credentials).
		fmt.Fprintf(
			os.Stderr,
			"Warning: this replaced an existing seed. The previous credential for worker %s is likely still"+
				" enrolled on the server; revoke it first or the enroll command above will fail:\n"+
				"  sqi-server worker revoke %s\n",
			workerID, workerID,
		)
	}
	return nil
}
