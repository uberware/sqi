// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/bus"
)

// Files `nats external init` writes, relative to --out.
const (
	natsIssuerSeedFile = "issuer.nk"
	natsDataSeedFile   = "sqi-data.nk"
	natsAuthSeedFile   = "sqi-auth.nk"
	natsSysSeedFile    = "sqi-sys.nk"
	natsFragmentFile   = "sqi-accounts.conf"

	// natsExternalPlaceholderURL stands in for the cluster's client URL in the
	// printed configuration, which cannot know it.
	natsExternalPlaceholderURL = "nats://nats.example.internal:4222"
)

var natsCmd = &cobra.Command{
	Use:   "nats",
	Short: "Set up the NATS broker sqi-server talks to",
}

var natsExternalCmd = &cobra.Command{
	Use:   "external",
	Short: "Set up an operator-run NATS server or cluster",
}

var natsExternalInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate the keys and config for an external NATS cluster",
	Long: `Generate everything needed to point sqi-server at a nats-server cluster you run,
with worker authentication through the auth callout.

Writes into the output directory:

  issuer.nk          account key sqi signs worker credentials with
  sqi-data.nk        user key of sqi's data connection (SQI account)
  sqi-auth.nk        user key of sqi's callout connection (AUTH account)
  sqi-sys.nk         user key of the system-account user that lets sqi
                     disconnect a revoked worker at once
  sqi-accounts.conf  nats-server config: the accounts and the auth_callout block

Include sqi-accounts.conf in every node's config, and put the printed nats:
block in sqi-server.yaml with your cluster's URL(s). The seeds are private keys
and never leave the machine that runs sqi-server; sqi-accounts.conf holds only
public keys and goes to the cluster.

Existing files are never replaced without --force, because a regenerated key
no longer matches the fragment already loaded by the cluster.`,
	Args: cobra.NoArgs,
	RunE: runNATSExternalInit,
}

var (
	natsExternalInitOut     string
	natsExternalInitAccount string
	natsExternalInitForce   bool
)

func init() {
	natsExternalInitCmd.Flags().StringVar(&natsExternalInitOut, "out", "./nats",
		"directory to write the seeds and the config fragment into")
	natsExternalInitCmd.Flags().StringVar(&natsExternalInitAccount, "account", bus.DefaultAccount,
		"name of the NATS account sqi's streams and workers live in")
	natsExternalInitCmd.Flags().BoolVar(&natsExternalInitForce, "force", false,
		"replace files that already exist")
	natsExternalCmd.AddCommand(natsExternalInitCmd)
	natsCmd.AddCommand(natsExternalCmd)
}

// natsExternalKeys is the key material of one run.
type natsExternalKeys struct {
	issuer, data, auth, sys []byte
	fragment                string
}

// generateNATSExternalKeys creates the keys and renders the fragment for
// account. It writes nothing, so a rejected account name leaves no files.
func generateNATSExternalKeys(account string) (natsExternalKeys, error) {
	var (
		k      natsExternalKeys
		layout = bus.Layout{Account: account, AuthEnabled: true}
		sysPub string
		err    error
	)
	if k.issuer, layout.IssuerPub, err = brokerauth.GenerateAccountSeed(); err != nil {
		return k, err
	}
	if k.data, layout.DataUserPub, err = brokerauth.GenerateSeed(); err != nil {
		return k, err
	}
	if k.auth, layout.CalloutUserPub, err = brokerauth.GenerateSeed(); err != nil {
		return k, err
	}
	if k.sys, sysPub, err = brokerauth.GenerateSeed(); err != nil {
		return k, err
	}
	if k.fragment, err = layout.ConfigFragmentWithSystemUser(sysPub); err != nil {
		return k, fmt.Errorf("invalid --account: %w", err)
	}
	return k, nil
}

func runNATSExternalInit(cmd *cobra.Command, _ []string) error {
	out, err := filepath.Abs(natsExternalInitOut)
	if err != nil {
		return fmt.Errorf("resolve --out: %w", err)
	}
	if !natsExternalInitForce {
		if err := refuseExistingNATSFiles(out); err != nil {
			return err
		}
	}
	keys, err := generateNATSExternalKeys(natsExternalInitAccount)
	if err != nil {
		return err
	}

	seeds := []struct {
		name string
		seed []byte
	}{
		{natsIssuerSeedFile, keys.issuer},
		{natsDataSeedFile, keys.data},
		{natsAuthSeedFile, keys.auth},
		{natsSysSeedFile, keys.sys},
	}
	for _, s := range seeds {
		if err := brokerauth.SaveSeed(filepath.Join(out, s.name), s.seed); err != nil {
			return err
		}
	}
	// The fragment holds public keys only, and goes to the cluster's nodes.
	fragment := filepath.Join(out, natsFragmentFile)
	if err := os.WriteFile(fragment, []byte(keys.fragment), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", fragment, err)
	}

	printNATSExternalInit(cmd.OutOrStdout(), out, natsExternalInitAccount)
	return nil
}

// refuseExistingNATSFiles reports an error naming every file in dir that a
// run would replace. Checking them all first keeps a refused run from leaving
// a mix of old and new keys.
func refuseExistingNATSFiles(dir string) error {
	var existing []string
	for _, name := range []string{
		natsIssuerSeedFile, natsDataSeedFile, natsAuthSeedFile, natsSysSeedFile, natsFragmentFile,
	} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		}
	}
	if len(existing) == 0 {
		return nil
	}
	return errors.New(strings.Join(existing, ", ") + " already exist(s); pass --force to replace them " +
		"(a regenerated key no longer matches the fragment your cluster already loaded)")
}

// printNATSExternalInit reports the files written and prints what to do with
// them. The sqi-server.yaml block comes last, so it can be copied to the end.
func printNATSExternalInit(w io.Writer, dir, account string) {
	path := func(name string) string { return filepath.Join(dir, name) }

	fmt.Fprintf(w, "Wrote:\n")
	for _, name := range []string{natsIssuerSeedFile, natsDataSeedFile, natsAuthSeedFile, natsSysSeedFile} {
		fmt.Fprintf(w, "  %s (private key, owner-only)\n", path(name))
	}
	fmt.Fprintf(w, "  %s (public keys only)\n", path(natsFragmentFile))

	// nats-server resolves an include path against the including file's
	// directory even when it is absolute, so the fragment is named relative
	// and the operator is told to put it beside each node's config.
	fmt.Fprintf(w, "\nCopy %s into the directory of every nats-server node's config file,\n", natsFragmentFile)
	fmt.Fprintf(w, "and include it from that config:\n\n")
	fmt.Fprintf(w, "  include %q\n", natsFragmentFile)
	fmt.Fprintf(w, "\nThe fragment defines the %s, AUTH and SYS accounts and the auth_callout block.\n", account)
	fmt.Fprintf(w, "Your config supplies the rest: listeners, TLS, clustering and the JetStream store_dir.\n")

	fmt.Fprintf(w, "\nOn the sqi-server host, add this to sqi-server.yaml and set the URL(s) of your cluster:\n\n")
	fmt.Fprintf(w, "nats:\n")
	fmt.Fprintf(w, "  mode: external\n")
	fmt.Fprintf(w, "  auth:\n")
	fmt.Fprintf(w, "    enabled: true\n")
	fmt.Fprintf(w, "  external:\n")
	fmt.Fprintf(w, "    urls:\n")
	fmt.Fprintf(w, "      - %q\n", natsExternalPlaceholderURL)
	fmt.Fprintf(w, "    account: %q\n", account)
	fmt.Fprintf(w, "    nkey_seed_file: %q\n", path(natsDataSeedFile))
	fmt.Fprintf(w, "    callout:\n")
	fmt.Fprintf(w, "      nkey_seed_file: %q\n", path(natsAuthSeedFile))
	fmt.Fprintf(w, "      issuer_seed_file: %q\n", path(natsIssuerSeedFile))
	fmt.Fprintf(w, "      system_creds_file: %q\n", path(natsSysSeedFile))
}
