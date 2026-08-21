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

	if !keygenFlags.Force {
		if _, err := os.Stat(seedPath); err == nil {
			return fmt.Errorf(
				"a credential already exists at %s; use --force to overwrite it (this invalidates the existing enrollment)",
				seedPath,
			)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", seedPath, err)
		}
	}

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
	return nil
}
