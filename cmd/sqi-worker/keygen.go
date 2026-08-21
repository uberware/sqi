// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/brokerauth"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

var keygenFlags struct {
	DataDir string
	Force   bool
}

var keygenCmd = &cobra.Command{
	Use:   "keygen",
	Short: "Generate a new nkey broker credential for this worker",
	Long: `Generate a new Ed25519 nkey keypair for authenticating this worker to
sqi-server's NATS broker, and write the private seed to the path resolved
from nats.credential_file (which defaults to <data-dir>/worker.nk), mode
0600.

Configuration is loaded the same way "sqi-worker start" loads it: the root
-c/--config file, SQI_WORKER_* environment variables, and built-in defaults.
Run this on the worker host with its normal config so the worker ID and
credential path match what that worker actually uses. --data-dir overrides
worker.data_dir explicitly, for a one-off run against a different directory.

Refuses to overwrite an existing seed unless --force is given — generating a
new keypair for a worker that is already enrolled invalidates its current
credential; the broker will reject connections signed with the old key until
the worker is re-enrolled with the new public key.

Prints the public key, whether the worker ID is an existing one loaded from
the data directory or one newly generated there, and the exact
"sqi-server worker enroll" command an operator must run on the server to
authorize this worker. This is the manual enrollment path — a worker that
can reach sqi-server's REST API can instead enroll itself automatically with
a join token issued by "sqi-server worker token issue".`,
	RunE: runKeygen,
}

func init() {
	keygenCmd.Flags().StringVar(
		&keygenFlags.DataDir,
		"data-dir", "",
		"override worker.data_dir (also moves the default credential path unless nats.credential_file is set explicitly)",
	)
	keygenCmd.Flags().BoolVar(
		&keygenFlags.Force,
		"force", false,
		"overwrite an existing seed file",
	)
}

func runKeygen(cmd *cobra.Command, _ []string) error {
	cfg, err := workerconfig.Load(persistentFlags.ConfigFile, flagOverrides())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	dataDir, seedPath := resolveKeygenPaths(cmd, cfg)
	if dataDir == "" {
		return errors.New("worker data directory is empty; use --data-dir or set worker.data_dir")
	}
	if seedPath == "" {
		return errors.New("credential file path is empty; use --data-dir or set nats.credential_file")
	}

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

	idPath := workerconfig.WorkerIDFilePath(dataDir)
	_, idStatErr := os.Stat(idPath)
	workerIDExisted := idStatErr == nil
	if idStatErr != nil && !errors.Is(idStatErr, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", idPath, idStatErr)
	}

	workerID, err := workerconfig.LoadOrCreateWorkerID(dataDir)
	if err != nil {
		return fmt.Errorf("load or create worker id: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Public key: %s\n", publicKey)
	if workerIDExisted {
		fmt.Fprintf(os.Stdout, "Worker ID: %s (existing, loaded from %s)\n", workerID, idPath)
	} else {
		fmt.Fprintf(
			os.Stdout,
			"Worker ID: %s (newly generated; if you expected an existing worker id here, "+
				"--data-dir/worker.data_dir is probably pointed at the wrong directory)\n",
			workerID,
		)
	}
	fmt.Fprintln(os.Stdout, "On the server, run:")
	fmt.Fprintf(os.Stdout, "  sqi-server worker enroll --worker-id %s --public-key %s\n", workerID, publicKey)
	fmt.Fprintln(os.Stdout, "A RUNNING sqi-server will not accept this credential until it restarts;"+
		" to enroll against a running server, use POST /api/v1/workers/enroll with a join token instead.")

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

// resolveKeygenPaths returns the worker data directory and credential seed
// path to use, applying --data-dir as an explicit override of the loaded
// cfg.Worker.DataDir.
//
// When --data-dir is passed and nats.credential_file was left at its
// config-derived default (rather than set explicitly, directly or via
// SQI_WORKER_NATS_CREDENTIAL_FILE), the seed path is re-derived under the
// overridden directory too, so it still lands at <data-dir>/worker.nk
// instead of the pre-override location. An explicitly configured
// nats.credential_file is left untouched.
func resolveKeygenPaths(cmd *cobra.Command, cfg workerconfig.WorkerConfig) (dataDir, seedPath string) {
	dataDir = cfg.Worker.DataDir
	seedPath = cfg.NATS.CredentialFile

	if cmd == nil || !cmd.Flags().Changed("data-dir") {
		return dataDir, seedPath
	}

	if seedPath == workerconfig.DefaultCredentialFile(dataDir) {
		seedPath = workerconfig.DefaultCredentialFile(keygenFlags.DataDir)
	}
	dataDir = keygenFlags.DataDir
	return dataDir, seedPath
}
